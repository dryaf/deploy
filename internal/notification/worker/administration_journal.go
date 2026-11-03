package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func (v MessageDecisionRecord) Validate() error {
	if err := v.Decision.Validate(); err != nil {
		return err
	}
	d := v.Decision
	state := "cancelled"
	if d.Action == "retry" {
		state = "queued"
		if protocol.Guarded(d.Review.Job.Message.Kind) {
			state = "waiting_status"
		}
	}
	if v.Version != 1 || v.Receipt.Position <= 0 || v.Receipt.EventID != d.Review.Job.Message.ID || v.Receipt.Sequence != d.Review.Job.Message.Sequence || v.Receipt.State != state || v.Receipt.AttemptID != "" || v.Receipt.Provider != "" || v.Receipt.Acceptance != nil || !v.Receipt.At.Equal(v.RecordedAt) || v.RecordedAt.Before(d.Review.CreatedAt) || !v.RecordedAt.Before(d.Review.ExpiresAt) {
		return fmt.Errorf("invalid administrative decision completion")
	}
	return nil
}

func (a messageAdministrator) readRecord(ctx context.Context, tx *sql.Tx, id string) (v MessageDecisionRecord, exists bool, err error) {
	if !safefs.ValidID(id, "n") {
		return v, false, fmt.Errorf("invalid administrative decision ID")
	}
	var body string
	err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", messageDecisionKey(id)).Scan(&body)
	var file MessageDecisionRecord
	fileErr := a.s.dir.ReadJSON(messageResultFile(id), &file)
	archived := false
	if err == sql.ErrNoRows {
		var intent MessageDecision
		e := a.s.dir.ReadJSON(messageIntentFile(id), &intent)
		if e == nil && intent.Review.Identity != a.s.identity {
			key := "message-decision-archive-v1:" + protocol.Hash(intent.Review.Identity) + ":" + id
			err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&body)
			archived = err == nil
		} else if e != nil && !os.IsNotExist(e) {
			return v, false, e
		}
	}
	if err == sql.ErrNoRows {
		if fileErr == nil {
			return v, false, fmt.Errorf("administrative completion lost its database record; recovery required")
		}
		if !os.IsNotExist(fileErr) {
			return v, false, fileErr
		}
		return v, false, nil
	}
	if err != nil {
		return v, false, err
	}
	if err = protocol.Decode([]byte(body), &v, 2*protocol.MaxRequest); err != nil {
		return v, false, err
	}
	if err = v.Validate(); err != nil {
		return v, false, err
	}
	if v.Decision.Review.ID != id {
		return v, false, fmt.Errorf("administrative decision key mismatch")
	}
	var intent MessageDecision
	if err = a.s.dir.ReadJSON(messageIntentFile(id), &intent); err != nil {
		return v, false, err
	}
	if protocol.Hash(intent) != protocol.Hash(v.Decision) {
		return v, false, fmt.Errorf("administrative completion differs from intent")
	}
	if fileErr != nil && !os.IsNotExist(fileErr) {
		return v, false, fileErr
	}
	if fileErr == nil && protocol.Hash(file) != protocol.Hash(v) {
		return v, false, fmt.Errorf("administrative completion file changed")
	}
	if !archived {
		if err = verifyMessageDecisionReceipt(ctx, tx, v); err != nil {
			return v, false, err
		}
	}
	return v, true, nil
}

func (a messageAdministrator) reserveHistory(ctx context.Context, tx *sql.Tx, id string) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE "+administrativeKeysPredicate).Scan(&count); err != nil {
		return err
	}
	if count >= maxMessageDecisions {
		return fmt.Errorf("administrative history limit reached")
	}
	ids, err := a.historyFiles()
	if err != nil {
		return err
	}
	ids[id] = true
	if len(ids) > maxMessageDecisions {
		return fmt.Errorf("administrative history limit reached")
	}
	return nil
}

func (a messageAdministrator) historyFiles() (map[string]bool, error) {
	ids := map[string]bool{}
	d, err := a.s.dir.Sub("administration")
	if os.IsNotExist(err) {
		return ids, nil
	}
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.File().Readdirnames(2*maxMessageDecisions + 33)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > 2*maxMessageDecisions+32 {
		return nil, fmt.Errorf("administrative file budget exceeded")
	}
	temporary := 0
	for _, name := range names {
		if safefs.IsJSONTemporary(name) {
			temporary++
			if temporary > 32 {
				return nil, fmt.Errorf("administrative temporary file budget exceeded")
			}
			continue
		}
		id := strings.TrimSuffix(strings.TrimSuffix(name, ".intent.json"), ".result.json")
		if !safefs.ValidID(id, "n") || (name != id+".intent.json" && name != id+".result.json") {
			return nil, fmt.Errorf("unknown administrative history file")
		}
		ids[id] = true
	}
	if len(ids) > maxMessageDecisions {
		return nil, fmt.Errorf("administrative history limit reached")
	}
	return ids, nil
}

// Check both halves before ordinary startup can grant anything. A surviving SQL
// completion repairs only its acknowledgement file. Pending file intents never
// apply themselves; they require the exact explicit decision and a current review.
func (s *Store) checkMessageAdministration(ctx context.Context) error {
	a := messageAdministrator{s: s}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := verifyAdministrativeHistory(ctx, tx, s.identity); err != nil {
			return err
		}
		ids, err := a.historyFiles()
		if err != nil {
			return err
		}
		keys, err := administrativeKeys(ctx, tx)
		if err != nil {
			return err
		}
		for _, key := range keys {
			ids[key[strings.LastIndex(key, ":")+1:]] = true
		}
		if len(ids) > maxMessageDecisions {
			return fmt.Errorf("administrative history budget exceeded")
		}
		for id := range ids {
			v, exists, e := a.readRecord(ctx, tx, id)
			if e != nil {
				return e
			}
			if exists {
				if e = a.publishResult(v); e != nil {
					return e
				}
				continue
			}
			var intent MessageDecision
			if e = s.dir.ReadJSON(messageIntentFile(id), &intent); e != nil {
				return e
			}
			if e = intent.Validate(); e != nil {
				return e
			}
			if intent.Review.ID != id || intent.Review.Identity.Worker != s.identity.Worker {
				return fmt.Errorf("administrative intent key changed")
			}
		}
		return nil
	})
}

func (a messageAdministrator) publishResult(v MessageDecisionRecord) error {
	if err := v.Validate(); err != nil {
		return err
	}
	var saved MessageDecisionRecord
	err := a.s.dir.ReadJSON(messageResultFile(v.Decision.Review.ID), &saved)
	if err == nil {
		if protocol.Hash(saved) != protocol.Hash(v) {
			return fmt.Errorf("administrative completion changed")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err = a.s.dir.WriteJSON(messageResultFile(v.Decision.Review.ID), v, false); err != nil {
		return err
	}
	return a.boundary("administration_result")
}

func (a messageAdministrator) status(ctx context.Context, id string) (v MessageDecisionRecord, err error) {
	err = a.s.transaction(ctx, func(tx *sql.Tx) error {
		var exists bool
		var e error
		v, exists, e = a.readRecord(ctx, tx, id)
		if e != nil {
			return e
		}
		if !exists {
			return fmt.Errorf("administrative decision is not committed")
		}
		return nil
	})
	return
}
