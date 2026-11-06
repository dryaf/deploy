package worker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

type ReplySender interface {
	SendReply(context.Context, interaction.Destination, string) provider.ReplyResult
}
type replyAttempt struct {
	ID    string
	Reply CommandReply
}

func (s *Store) grantReply(ctx context.Context, r CommandReply) (a replyAttempt, ok bool, err error) {
	if !s.commandRepliesEnabled() {
		return a, false, nil
	}
	if err = s.checkManagedCommandAuthority(ctx); err != nil {
		return a, false, err
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var state, body string
		var due int64
		if err := tx.QueryRow("SELECT body,state,due FROM command_replies WHERE command_id=? AND kind=? AND revision=?", r.CommandID, r.Kind, r.Revision).Scan(&body, &state, &due); err != nil {
			return err
		}
		if state != "pending" || due > s.now().UnixNano() || !s.clockSafe() {
			return nil
		}
		var current CommandReply
		if err := protocol.Decode([]byte(body), &current, protocol.MaxRequest); err != nil {
			return err
		}
		if protocol.Hash(current) != protocol.Hash(r) {
			return fmt.Errorf("reply changed")
		}
		var blocked, obsolete bool
		owner := ""
		if r.Kind == "gateway" {
			allowed, e := s.gatewayReplyAllowed(tx, r)
			if e != nil {
				return e
			}
			blocked = !allowed
		} else {
			v, e := readCommand(tx, r.CommandID)
			if e != nil {
				return e
			}
			if e = expireCommand(tx, &v, s.now()); e != nil {
				return e
			}
			owner = v.Command.Producer.ID
			blocked = !s.commandAuthorized(v.Command, r.Scope) || v.Conflict
			obsolete = (r.Kind == "result" && r.Revision < v.Revision) || (r.Kind != "terminal" && v.State == "expired") || (r.Kind == "result" && v.Prepared != nil && r.Revision == 1 && v.State != "needs_confirmation")
		}
		if blocked || obsolete {
			state = "blocked"
			if obsolete {
				state = "superseded"
			}
			_, err = tx.Exec("UPDATE command_replies SET state=? WHERE command_id=? AND kind=? AND revision=?", state, r.CommandID, r.Kind, r.Revision)
			return err
		}
		if err = s.capacity(tx, owner, len(body)+8192, false); err != nil {
			return err
		}
		a.ID, err = safefs.NewID("reply")
		if err != nil {
			return err
		}
		a.Reply = r
		if _, err = tx.Exec("INSERT INTO command_reply_attempts(id,command_id,kind,revision,body,state) VALUES(?,?,?,?,?,'sending')", a.ID, r.CommandID, r.Kind, r.Revision, body); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE command_replies SET state='sending',attempt=?,tries=tries+1 WHERE command_id=? AND kind=? AND revision=?", a.ID, r.CommandID, r.Kind, r.Revision); err != nil {
			return err
		}
		ok = true
		return nil
	})
	if err != nil {
		ok = false
	}
	return
}

func (s *Store) finishReply(ctx context.Context, a replyAttempt, result provider.ReplyResult) error {
	if result.Accepted && !protocol.ValidToken(result.NativeID) {
		result = provider.ReplyResult{Result: provider.Result{Ambiguous: true}}
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var attempt string
		var tries int
		if err := tx.QueryRow("SELECT attempt,tries FROM command_replies WHERE command_id=? AND kind=? AND revision=?", a.Reply.CommandID, a.Reply.Kind, a.Reply.Revision).Scan(&attempt, &tries); err != nil {
			return err
		}
		if attempt != a.ID {
			return fmt.Errorf("reply attempt no longer owned")
		}
		state := "pending"
		due := s.now().Add(time.Duration(tries) * time.Minute)
		switch {
		case result.Accepted:
			state = "provider_accepted"
		case result.Ambiguous:
			state = "ambiguous"
		case result.Permanent || tries >= 3:
			state = "blocked"
		case result.RetryAfter > 0 && result.RetryAfter <= 24*time.Hour:
			due = s.now().Add(result.RetryAfter)
		}
		if _, err := tx.Exec("UPDATE command_reply_attempts SET state=?,native_id=? WHERE id=?", state, result.NativeID, a.ID); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE command_replies SET state=?,attempt='',due=? WHERE command_id=? AND kind=? AND revision=?", state, due.UnixNano(), a.Reply.CommandID, a.Reply.Kind, a.Reply.Revision)
		return err
	})
}

// DispatchCommandReplies has its own bounded queue; it never selects an alert
// route or fallback recipient. Provider I/O runs outside database transactions.
func (s *Store) DispatchCommandReplies(ctx context.Context, sender ReplySender) error {
	ctx, cancelBudget := context.WithTimeout(ctx, 20*time.Second)
	defer cancelBudget()
	replies, err := s.PendingCommandReplies(ctx)
	if err != nil {
		return err
	}
	for _, r := range replies {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a, ok, err := s.grantReply(ctx, r)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		result := sender.SendReply(attemptCtx, r.Destination, r.Text+fmt.Sprintf("\n[%s %s revision %d]", r.CommandID, r.Kind, r.Revision))
		cancel()
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		err = s.finishReply(finishCtx, a, result)
		finishCancel()
		if err != nil {
			return err
		}
	}
	return nil
}
