package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// RestoredRecoveryInbox exports immutable old-epoch evidence while the new relay
// remains quarantined. It cannot receive, ACK, install a candidate or enable intake.
// The managed caller must separately drain all Signal account consumers.
type RestoredRecoveryInbox struct {
	mu         sync.Mutex
	dir, stage *safefs.Directory
	lease      *safefs.Lock
	journal    InboxRestoreState
	fault      func(string) error
	history    []interaction.SignalRange
}

var _ interaction.SignalRecoverySource = (*RestoredRecoveryInbox)(nil)

func OpenRestoredRecoveryInbox(ctx context.Context, path, account string) (_ *RestoredRecoveryInbox, err error) {
	r := &RestoredRecoveryInbox{}
	defer func() {
		if err != nil {
			r.Close()
		}
	}()
	r.dir, err = privateInboxDirectory(path)
	if err != nil {
		return nil, err
	}
	if err = absentRuntimeFile(r.dir, runtimeInstallIntent); err != nil {
		return nil, err
	}
	r.lease, err = r.dir.Lock("inbox.lock", true, false)
	if err != nil {
		return nil, err
	}
	if err = r.dir.ReadJSON("restore.json", &r.journal); err != nil {
		return nil, err
	}
	if err = r.journal.validate(account); err != nil {
		return nil, err
	}
	if r.journal.Phase != "quarantined" {
		return nil, fmt.Errorf("relay candidate is not quarantined")
	}
	r.stage, err = r.dir.Sub("restore/" + r.journal.Operation)
	if err != nil {
		return nil, err
	}
	if err = r.check(ctx); err != nil {
		return nil, err
	}
	if r.journal.Snapshot.Schema == runtimeInboxSchema {
		db, e := inboxArtifactDB(r.stage, "source.db", true)
		if e != nil {
			return nil, e
		}
		defer db.Close()
		proof, e := readInboxBaselineProof(db, inboxPin{Identity: r.journal.Before, Account: account, Baseline: r.journal.Snapshot.BaselineHash})
		if e != nil {
			return nil, e
		}
		m := r.manifest()
		if e = addBaselineHistory(&m, *proof); e != nil {
			return nil, e
		}
		if e = m.Validate(); e != nil {
			return nil, e
		}
		r.history = m.History
	}
	return r, nil
}

func (r *RestoredRecoveryInbox) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stage != nil {
		r.stage.Close()
		r.stage = nil
	}
	if r.lease != nil {
		r.lease.Close()
		r.lease = nil
	}
	if r.dir != nil {
		r.dir.Close()
		r.dir = nil
	}
	return nil
}
func (r *RestoredRecoveryInbox) Cursor() interaction.SpoolCursor {
	// The source is old history, never a claim that a new-epoch cursor is active.
	return interaction.SpoolCursor{Identity: r.journal.Before, Account: r.journal.Account}
}
func (r *RestoredRecoveryInbox) manifest() interaction.SignalRecoveryManifest {
	j, m := r.journal, r.journal.Snapshot
	v := interaction.SignalRecoveryManifest{
		Relay: r.Cursor(), Highest: m.Highest, Acknowledged: m.Acknowledged, Listed: m.Listed, Digest: m.EventsDigest,
		Restore: interaction.SignalRelayRestore{Operation: j.Operation, Snapshot: m.ID, SnapshotHash: m.DatabaseHash, After: j.After},
	}
	if m.Schema == runtimeInboxSchema {
		v.BaselineHash, v.BaselinePosition, v.RetainedEvents = m.BaselineHash, m.BaselinePosition, m.RetainedEvents
		v.History = append([]interaction.SignalRange(nil), r.history...)
	}
	return v
}
func (r *RestoredRecoveryInbox) check(ctx context.Context) error {
	for _, name := range []string{runtimeInstallIntent, runtimeInstallSeal, runtimeInstallComplete} {
		if err := absentRuntimeFile(r.dir, name); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.lease == nil || r.lease.File() == nil {
		return fmt.Errorf("relay restore lease closed")
	}
	held, err := r.lease.File().Stat()
	if err != nil {
		return err
	}
	f, err := r.dir.Open("inbox.lock", os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	current, err := f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	if !os.SameFile(held, current) {
		return fmt.Errorf("relay restore lock replaced")
	}
	var j InboxRestoreState
	if err = r.dir.ReadJSON("restore.json", &j); err != nil {
		return err
	}
	if j != r.journal {
		return fmt.Errorf("relay restore journal changed")
	}
	var pin inboxPin
	if err = r.dir.ReadJSON("identity.json", &pin); err != nil {
		return err
	}
	if pin != (inboxPin{Identity: j.After, Account: j.Account, RecoveryOperation: j.Operation}) {
		return fmt.Errorf("relay restore identity changed")
	}
	if err = verifyStandaloneInboxFile(r.stage, "source.db", j.Snapshot.DatabaseBytes, j.Snapshot.DatabaseHash); err != nil {
		return err
	}
	if err = verifyStandaloneInboxFile(r.stage, "candidate.db", j.CandidateBytes, j.CandidateHash); err != nil {
		return err
	}
	return ctx.Err()
}
func (r *RestoredRecoveryInbox) validate(ch interaction.SignalRecoveryChallenge) error {
	if ch.Relay != r.Cursor() {
		return fmt.Errorf("restored relay source identity mismatch")
	}
	return ch.Validate(time.Now().UTC(), ch.BootID, ch.Uptime)
}
func (r *RestoredRecoveryInbox) readExport() (s recoveryExport, err error) {
	err = r.stage.ReadJSON("gateway-export.json", &s)
	if err != nil {
		return
	}
	if !s.Manifest.Equal(r.manifest()) || s.Challenge.Relay != r.Cursor() ||
		(r.journal.GatewayExport != "" && s.Challenge.Operation != r.journal.GatewayExport) {
		return s, fmt.Errorf("restored relay export binding changed")
	}
	// Expired completed evidence remains readable; page/import checks require a
	// fresh challenge. Validate structure without renewing any lease.
	if err = s.Challenge.Validate(s.Challenge.IssuedAt, s.Challenge.BootID, s.Challenge.Uptime); err != nil {
		return
	}
	if s.Summary != nil {
		if err = s.Summary.Validate(); err != nil {
			return
		}
		if !s.Summary.Manifest.Equal(s.Manifest) || protocol.Hash(s.Summary.Challenge) != protocol.Hash(s.Challenge) {
			return s, fmt.Errorf("restored relay completion binding changed")
		}
	}
	if r.journal.GatewaySummary != "" && (s.Summary == nil || protocol.Hash(*s.Summary) != r.journal.GatewaySummary) {
		return s, fmt.Errorf("restored relay completion lost or rolled back")
	}
	return
}

func (r *RestoredRecoveryInbox) Capture(ctx context.Context, ch interaction.SignalRecoveryChallenge) (m interaction.SignalRecoveryManifest, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err = r.check(ctx); err != nil {
		return
	}
	if err = r.validate(ch); err != nil {
		return
	}
	s, err := r.readExport()
	if os.IsNotExist(err) && r.journal.GatewayExport == "" {
		s = recoveryExport{Challenge: ch, Manifest: r.manifest()}
		if err = r.stage.WriteJSON("gateway-export.json", s, false); err != nil {
			return
		}
		if err = inboxRestoreBoundary(r.fault, "relay_restore_export_file"); err != nil {
			return
		}
	} else if err != nil {
		return
	}
	if s.Challenge.Operation != ch.Operation || s.Challenge.Gateway != ch.Gateway ||
		ch.IssuedAt.Before(s.Challenge.IssuedAt) ||
		(ch.Nonce == s.Challenge.Nonce && protocol.Hash(ch) != protocol.Hash(s.Challenge)) ||
		(s.Summary != nil && protocol.Hash(ch) != protocol.Hash(s.Challenge)) {
		return m, fmt.Errorf("restored relay export authority changed")
	}
	if r.journal.GatewayExport == "" {
		next := r.journal
		next.GatewayExport = ch.Operation
		if err = r.dir.WriteJSON("restore.json", next, true); err != nil {
			return
		}
		r.journal.GatewayExport = ch.Operation
		if err = inboxRestoreBoundary(r.fault, "relay_restore_export_pin"); err != nil {
			return
		}
	}
	s.Challenge = ch
	if err = r.stage.WriteJSON("gateway-export.json", s, true); err != nil {
		return
	}
	m = s.Manifest
	return
}

func (r *RestoredRecoveryInbox) Page(ctx context.Context, ch interaction.SignalRecoveryChallenge, after uint64) (p interaction.SignalRecoveryPage, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err = r.check(ctx); err != nil {
		return
	}
	if err = r.validate(ch); err != nil {
		return
	}
	s, err := r.readExport()
	if err != nil {
		return
	}
	if r.journal.GatewayExport != ch.Operation || protocol.Hash(s.Challenge) != protocol.Hash(ch) || after > s.Manifest.EventCount() {
		return p, fmt.Errorf("restored relay page authority changed")
	}
	db, err := inboxArtifactDB(r.stage, "source.db", true)
	if err != nil {
		return
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT position,key,hash,body FROM events ORDER BY position LIMIT 128 OFFSET ?", after)
	if err != nil {
		return
	}
	defer rows.Close()
	p.Next = after
	size := 2
	for rows.Next() {
		var position uint64
		var key, hash, body string
		var e interaction.SpoolEvent
		if err = rows.Scan(&position, &key, &hash, &body); err != nil {
			return
		}
		if err = protocol.Decode([]byte(body), &e, 16384); err != nil {
			return
		}
		if position != s.Manifest.PositionAt(p.Next) || position > s.Manifest.Highest || e.Position != 0 || e.Key != key || e.Hash != hash {
			return p, fmt.Errorf("restored relay event identity changed")
		}
		e.Position = position
		if err = interaction.ValidateSignalRecoveryEvent(e, r.Cursor()); err != nil {
			return
		}
		raw, _ := json.Marshal(e)
		extra := len(raw)
		if len(p.Events) > 0 {
			extra++
		}
		if size+extra+4096 > protocol.MaxRequest {
			break
		}
		size += extra
		p.Events = append(p.Events, e)
		p.Next++
	}
	if err = rows.Err(); err != nil {
		return
	}
	p.Done = p.Next == s.Manifest.EventCount()
	if !p.Done && len(p.Events) == 0 {
		return p, fmt.Errorf("restored relay page missing")
	}
	err = r.check(ctx)
	return
}
func (r *RestoredRecoveryInbox) Completed(ctx context.Context, ch interaction.SignalRecoveryChallenge) (done bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err = r.check(ctx); err != nil {
		return
	}
	if err = r.validate(ch); err != nil {
		return
	}
	s, err := r.readExport()
	if os.IsNotExist(err) && r.journal.GatewayExport == "" {
		return false, nil
	}
	if err != nil {
		return
	}
	if s.Challenge.Operation != ch.Operation || s.Challenge.Gateway != ch.Gateway {
		return false, fmt.Errorf("restored relay recovery authority changed")
	}
	return s.Summary != nil, nil
}
func (r *RestoredRecoveryInbox) RecordSummary(ctx context.Context, summary interaction.SignalRecoverySummary) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(ctx); err != nil {
		return err
	}
	if err := summary.Validate(); err != nil {
		return err
	}
	s, err := r.readExport()
	if err != nil {
		return err
	}
	if r.journal.GatewayExport != summary.Challenge.Operation || !s.Manifest.Equal(summary.Manifest) ||
		protocol.Hash(s.Challenge) != protocol.Hash(summary.Challenge) ||
		(s.Summary != nil && protocol.Hash(*s.Summary) != protocol.Hash(summary)) {
		return fmt.Errorf("restored relay completion changed")
	}
	if err = inboxRestoreBoundary(r.fault, "relay_restore_summary_before"); err != nil {
		return err
	}
	s.Summary = &summary
	if err = r.stage.WriteJSON("gateway-export.json", s, true); err != nil {
		return err
	}
	if err = inboxRestoreBoundary(r.fault, "relay_restore_summary_file"); err != nil {
		return err
	}
	next := r.journal
	next.GatewaySummary = protocol.Hash(summary)
	if err = r.dir.WriteJSON("restore.json", next, true); err != nil {
		return err
	}
	r.journal.GatewaySummary = next.GatewaySummary
	return inboxRestoreBoundary(r.fault, "relay_restore_summary_after")
}
