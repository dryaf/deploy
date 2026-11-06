package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func readCommand(tx *sql.Tx, id string) (v interaction.View, err error) {
	var body, grant, prepared, proof string
	var conflict int
	err = tx.QueryRow("SELECT body,state,grant,revision,preparation,conflict,non_admission FROM commands WHERE id=?", id).Scan(&body, &v.State, &grant, &v.Revision, &prepared, &conflict, &proof)
	if err != nil {
		return
	}
	err = protocol.Decode([]byte(body), &v.Command, protocol.MaxRequest)
	if err != nil {
		return
	}
	v.Conflict = conflict != 0
	if proof != "" {
		v.NonAdmission = &interaction.NonAdmissionProof{}
		if err = protocol.Decode([]byte(proof), v.NonAdmission, 8192); err != nil {
			return
		}
	}
	if grant != "" {
		v.Grant = &interaction.Grant{}
		err = protocol.Decode([]byte(grant), v.Grant, 8192)
		if err != nil {
			return
		}
	}
	if prepared != "" {
		v.Prepared = &interaction.Preparation{}
		err = protocol.Decode([]byte(prepared), v.Prepared, 8192)
	}
	return
}
func (s *Store) commandAuthorized(c interaction.Command, scope interaction.Scope) bool {
	config := s.commandConfig()
	if config == nil || c.PolicyRevision != protocol.Hash(config) {
		return false
	}
	current, ok := config.Authorize(c.Actor, c.Destination.ID, c.Producer.ID, c.Descriptor.Permission)
	if !ok || !current.Allows(scope) || !c.Scope.Allows(scope) {
		return false
	}
	for _, d := range config.Destinations {
		if d == c.Destination {
			return true
		}
	}
	return false
}

// AdmitCommand is called by authenticated provider adapters, not by an untrusted
// remote endpoint. The local API retains the base trusted-UID boundary.
func (s *Store) AdmitCommand(ctx context.Context, in interaction.Inbound) (v interaction.View, err error) {
	if s.verificationOnly {
		return v, fmt.Errorf("command intake unavailable in verification mode")
	}
	if !s.commandIntakeEnabled() {
		return v, fmt.Errorf("command intake is not admitted")
	}
	if err = s.checkManagedCommandAuthority(ctx); err != nil {
		return v, err
	}
	c := s.commandConfig()
	if c == nil {
		return v, fmt.Errorf("interactive intake disabled")
	}
	if !protocol.ValidToken(in.NativeID) || !protocol.ValidToken(in.Actor) || in.SentAt.IsZero() || len(in.Text) > 4096 || !utf8.ValidString(in.Text) {
		return v, fmt.Errorf("invalid inbound identity")
	}
	var destination interaction.Destination
	found := false
	for _, d := range c.Destinations {
		if d.ID == in.Conversation && d.Provider == in.Provider && d.Account == in.Account && d.Fingerprint() == in.DestinationFingerprint {
			destination = d
			found = true
		}
	}
	if !found {
		return v, interaction.Reject("conversation not enrolled")
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if err := guardRequestAdmission(tx, in); err != nil {
			return err
		}
		if err := s.reserveAliases(tx); err != nil {
			return err
		}
		if !s.clockSafe() {
			return fmt.Errorf("clock uncertain")
		}
		key := in.RequestKey()
		hash := protocol.Hash(in)
		var previous, id string
		err := tx.QueryRow("SELECT input_hash,command_id FROM command_inbox WHERE request_key=?", key).Scan(&previous, &id)
		if err == nil {
			if previous != hash {
				return fmt.Errorf("inbound identity conflict")
			}
			if strings.HasPrefix(id, "Q-") {
				q, e := readGatewayQuery(tx, id)
				if e != nil {
					return e
				}
				if q.Kind == "rejected" {
					return interaction.RejectQuietly("request already rejected")
				}
				return fmt.Errorf("inbound identity already used for a gateway query")
			}
			v, err = readCommand(tx, id)
			if err == nil && !s.commandEffectEnabled(v.Command.Descriptor.Effect) {
				return interaction.Reject("command effect is not admitted")
			}
			if err == nil && !s.commandAuthorized(v.Command, v.Command.Scope) {
				return interaction.Reject("permission changed")
			}
			return err
		}
		if err != sql.ErrNoRows {
			return err
		}
		if in.SentAt.After(s.now().Add(5*time.Second)) || s.now().Sub(in.SentAt) > 15*time.Minute {
			return interaction.Reject("stale inbound request")
		}
		if err = s.commandRate(tx, in); err != nil {
			return err
		}
		if strings.HasPrefix(in.Text, "confirm ") {
			return s.confirmCommand(tx, in, destination, &v)
		}
		parsed, bound, err := s.routeCommand(tx, in)
		if err != nil {
			return err
		}
		w, ok := c.Resolve(parsed.Worker)
		if !ok {
			return interaction.Reject("worker unavailable")
		}
		if err = s.requireUnretiredProducer(w.Producer); err != nil {
			return interaction.Reject("worker unavailable")
		}
		d, ok := w.Commands[parsed.Verb]
		if !ok {
			return interaction.Reject("command unavailable")
		}
		if !s.commandEffectEnabled(d.Effect) {
			return interaction.Reject("command effect is not admitted")
		}
		scope, ok := c.Authorize(in.Actor, destination.ID, w.Producer.ID, d.Permission)
		if !ok {
			return interaction.Reject("command not authorized")
		}
		if !d.Scoped && !scope.WholeWorker {
			return interaction.Reject("whole-worker permission required")
		}
		if err = interaction.ValidateArguments(d, parsed.Arguments); err != nil {
			return interaction.Reject(err.Error())
		}
		if subject := parsed.Arguments["subject"]; subject != "" && !scope.Contains(subject) {
			return interaction.Reject("subject not authorized")
		}
		if subject := parsed.Arguments["subject"]; subject != "" && d.Scoped {
			scope = interaction.Scope{Subjects: []string{subject}}
		}
		if bound != nil && d.Scoped {
			scope = contextScope(bound)
		}
		var total, own, actor int
		if err = tx.QueryRow("SELECT count(*),COALESCE(sum(producer=?),0),COALESCE(sum(json_extract(body,'$.actor')=? AND json_extract(body,'$.destination.account')=?),0) FROM commands WHERE state NOT IN ('succeeded','failed','rejected','expired')", w.Producer.ID, in.Actor, in.Account).Scan(&total, &own, &actor); err != nil {
			return err
		}
		if total >= 1024 || own >= 128 || actor >= 16 {
			return fmt.Errorf("command capacity exhausted")
		}
		// Rate limits count durable admissions, including already completed work.
		var recentActor, recentConversation int
		if err = tx.QueryRow("SELECT COALESCE(sum(json_extract(body,'$.actor')=? AND json_extract(body,'$.destination.account')=?),0),COALESCE(sum(json_extract(body,'$.destination.id')=?),0) FROM commands WHERE created>?", in.Actor, in.Account, destination.ID, s.now().Add(-time.Minute).UnixNano()).Scan(&recentActor, &recentConversation); err != nil {
			return err
		}
		if recentActor >= 10 || recentConversation >= 60 {
			return fmt.Errorf("command rate exceeded")
		}
		deadline := s.now().Add(15 * time.Minute)
		if d.Effect == "read" {
			deadline = s.now().Add(time.Minute)
		}
		cmd := interaction.Command{ID: in.CommandID(), RequestKey: key, InputHash: hash, Producer: w.Producer, Generation: w.Generation, Actor: in.Actor, Destination: destination, PolicyRevision: protocol.Hash(c), Descriptor: d, Arguments: parsed.Arguments, Scope: scope, IssuedAt: s.now().UTC(), Deadline: deadline.UTC()}
		cmd.Context = bound
		raw, _ := json.Marshal(cmd)
		if len(raw)+4096 > protocol.MaxRequest {
			return interaction.Reject("resolved command exceeds wire budget; select a smaller target scope")
		}
		if err = s.capacity(tx, w.Producer.ID, len(raw)+(protocol.MaxRequest*2), false); err != nil {
			return err
		}
		state := "queued"
		if d.Confirm {
			state = "received"
		}
		if _, err = tx.Exec("INSERT INTO commands(id,request_key,input_hash,producer,body,state,created) VALUES(?,?,?,?,?,?,?)", cmd.ID, key, hash, w.Producer.ID, string(raw), state, s.now().UnixNano()); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO command_inbox VALUES(?,?,?)", key, hash, cmd.ID); err != nil {
			return err
		}
		if err = s.queueCommandReply(tx, cmd, 0, "Request recorded: "+cmd.ID, scope); err != nil {
			return err
		}
		v = interaction.View{Command: cmd, State: state}
		return nil
	})
	return
}

func (s *Store) confirmCommand(tx *sql.Tx, in interaction.Inbound, d interaction.Destination, out *interaction.View) error {
	token := strings.TrimPrefix(in.Text, "confirm ")
	if !protocol.ValidToken(token) {
		return interaction.Reject("invalid confirmation; use confirm <token>")
	}
	var id string
	if err := tx.QueryRow("SELECT id FROM commands WHERE token_hash=?", protocol.Hash(token)).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return interaction.Reject("confirmation unavailable")
		}
		return err
	}
	if err := requireCurrentCommand(tx, id); err != nil {
		return err
	}
	v, err := readCommand(tx, id)
	if err != nil {
		return err
	}
	if !s.commandEffectEnabled(v.Command.Descriptor.Effect) {
		return interaction.Reject("command effect is not admitted")
	}
	if err = s.capacity(tx, v.Command.Producer.ID, 8192, false); err != nil {
		return err
	}
	if v.Command.Actor != in.Actor || v.Command.Destination != d || !s.commandAuthorized(v.Command, v.Command.Scope) {
		return fmt.Errorf("confirmation not authorized")
	}
	if err = s.confirmationReply(tx, in, v.Command); err != nil {
		return err
	}
	if v.State == "needs_confirmation" {
		if v.Prepared == nil || !v.Prepared.ExpiresAt.After(s.now()) || !v.Command.Deadline.After(s.now()) {
			return fmt.Errorf("confirmation expired")
		}
		if _, err = tx.Exec("UPDATE commands SET state='queued',confirmed=1 WHERE id=?", id); err != nil {
			return err
		}
		v.State = "queued"
	} else {
		var confirmed int
		if err = tx.QueryRow("SELECT confirmed FROM commands WHERE id=?", id).Scan(&confirmed); err != nil {
			return err
		}
		if confirmed == 0 {
			return fmt.Errorf("confirmation closed")
		}
	}
	if _, err = tx.Exec("INSERT INTO command_inbox VALUES(?,?,?)", in.RequestKey(), protocol.Hash(in), id); err != nil {
		return err
	}
	*out = v
	return nil
}

func expireCommand(tx *sql.Tx, v *interaction.View, now time.Time) error {
	if interaction.Terminal(v.State) || v.State == "running" || v.State == "outcome_unknown" {
		return nil
	}
	deadline := v.Command.Deadline
	if v.Prepared != nil && v.Prepared.ExpiresAt.Before(deadline) {
		deadline = v.Prepared.ExpiresAt
	}
	if deadline.After(now) {
		return nil
	}
	v.State = "expired"
	if v.Grant != nil {
		v.State = "outcome_unknown"
	}
	_, err := tx.Exec("UPDATE commands SET state=? WHERE id=?", v.State, v.Command.ID)
	return err
}

func (s *Store) Commands(ctx context.Context, p protocol.Producer) (out []interaction.View, err error) {
	if _, err = s.config.enrollment(p); err != nil {
		return
	}
	out = []interaction.View{}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if s.commandScanFloor == nil {
			s.commandScanFloor = map[string]int64{}
		}
		rows, err := tx.Query("SELECT id,rowid FROM commands WHERE producer=? AND state NOT IN ('succeeded','failed','expired','rejected') ORDER BY (rowid<=?),rowid LIMIT 128", p.ID, s.commandScanFloor[p.ID])
		if err != nil {
			return err
		}
		type item struct {
			id       string
			position int64
		}
		var ids []item
		for rows.Next() {
			var id item
			if err = rows.Scan(&id.id, &id.position); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		bytes := 2
		for _, id := range ids {
			historical, err := historicalCommand(tx, id.id)
			if err != nil {
				return err
			}
			if historical {
				s.commandScanFloor[p.ID] = id.position
				continue
			}

			v, err := readCommand(tx, id.id)
			if err != nil {
				return err
			}
			if v.Command.Producer != p {
				return fmt.Errorf("command epoch mismatch")
			}
			if err = expireCommand(tx, &v, s.now()); err != nil {
				return err
			}
			raw, _ := json.Marshal(v)
			if bytes+len(raw)+1 > protocol.MaxResponse-2048 {
				break
			}
			bytes += len(raw) + 1
			out = append(out, v)
			s.commandScanFloor[p.ID] = id.position
		}
		return nil
	})
	return
}
func (s *Store) ImportCommand(ctx context.Context, p protocol.Producer, id, hash string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := requireCurrentCommand(tx, id); err != nil {
			return err
		}
		v, err := readCommand(tx, id)
		if err != nil {
			return err
		}
		if v.Command.Producer != p || protocol.Hash(v.Command) != hash {
			return fmt.Errorf("command import mismatch")
		}
		_, err = tx.Exec("UPDATE commands SET imported=1 WHERE id=?", id)
		return err
	})
}

func (s *Store) GrantCommand(ctx context.Context, p protocol.Producer, id string) (grant interaction.Grant, err error) {
	if !s.commandIntakeEnabled() {
		return grant, fmt.Errorf("command execution is not admitted")
	}
	if err = s.checkManagedCommandAuthority(ctx); err != nil {
		return grant, err
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if err := requireCurrentCommand(tx, id); err != nil {
			return err
		}
		if err := s.requireUnretiredProducer(p); err != nil {
			return err
		}
		v, err := readCommand(tx, id)
		if err != nil {
			return err
		}
		if v.Command.Producer != p {
			return fmt.Errorf("producer mismatch")
		}
		if !s.commandEffectEnabled(v.Command.Descriptor.Effect) {
			return fmt.Errorf("command effect is not admitted")
		}
		if err = expireCommand(tx, &v, s.now()); err != nil {
			return err
		}
		// Expiration commits even when no new grant is returned.
		if v.State != "queued" || v.Conflict || !s.clockSafe() || !s.commandAuthorized(v.Command, v.Command.Scope) {
			return nil
		}
		var imported, confirmed int
		if err = tx.QueryRow("SELECT imported,confirmed FROM commands WHERE id=?", id).Scan(&imported, &confirmed); err != nil {
			return err
		}
		if imported == 0 || (v.Command.Descriptor.Confirm && confirmed == 0) {
			return nil
		}
		if v.Grant != nil {
			grant = *v.Grant
			return nil
		}
		gid, err := safefs.NewID("grant")
		if err != nil {
			return err
		}
		expires := s.now().Add(10 * time.Second)
		if v.Command.Deadline.Before(expires) {
			expires = v.Command.Deadline
		}
		if v.Prepared != nil && v.Prepared.ExpiresAt.Before(expires) {
			expires = v.Prepared.ExpiresAt
		}
		grant = interaction.Grant{Identity: s.identity, ID: gid, CommandID: id, CommandHash: protocol.Hash(v.Command), ExpiresAt: expires.UTC()}
		if v.Prepared != nil {
			grant.PreparationHash = v.Prepared.Hash
		}
		raw, _ := json.Marshal(grant)
		_, err = tx.Exec("UPDATE commands SET grant=? WHERE id=?", string(raw), id)
		return err
	})
	return
}

type CommandReply struct {
	Kind        string                  `json:"kind"`
	CommandID   string                  `json:"command_id"`
	Revision    uint64                  `json:"revision"`
	Destination interaction.Destination `json:"destination"`
	Scope       interaction.Scope       `json:"scope"`
	Text        string                  `json:"text"`
}

func (s *Store) queueCommandReply(tx *sql.Tx, c interaction.Command, revision uint64, text string, scope interaction.Scope) error {
	kind := "result"
	if revision == 0 {
		kind = "request"
	}
	return s.queueCommandReplyKind(tx, c, kind, revision, text, scope)
}
func (s *Store) queueCommandReplyKind(tx *sql.Tx, c interaction.Command, kind string, revision uint64, text string, scope interaction.Scope) error {
	if !s.commandAuthorized(c, scope) {
		return nil
	}
	raw, _ := json.Marshal(CommandReply{Kind: kind, CommandID: c.ID, Revision: revision, Destination: c.Destination, Scope: scope, Text: text})
	_, err := tx.Exec("INSERT INTO command_replies(command_id,kind,revision,body) VALUES(?,?,?,?)", c.ID, kind, revision, string(raw))
	return err
}

func (s *Store) SubmitCommandResult(ctx context.Context, p protocol.Producer, r interaction.Result) (receipt interaction.ResultReceipt, err error) {
	if err = r.Validate(); err != nil {
		return receipt, err
	}
	if !protocol.Sequence(r.Revision) || !protocol.ValidToken(r.CommandID) || r.Scope.Validate() != nil || !utf8.ValidString(r.Text) || utf8.RuneCountInString(r.Text) > 3000 {
		return receipt, fmt.Errorf("invalid command result")
	}
	if r.JobID != "" && !protocol.ValidToken(r.JobID) {
		return receipt, fmt.Errorf("invalid job identity")
	}
	if r.Phase != "prepare" && r.Phase != "execute" {
		return receipt, fmt.Errorf("invalid phase")
	}
	if r.Phase == "prepare" {
		if r.JobID != "" || (r.State != "needs_confirmation" && r.State != "rejected") || (r.State == "needs_confirmation" && r.Preparation == nil) {
			return receipt, fmt.Errorf("invalid preparation result")
		}
	} else if r.Preparation != nil {
		return receipt, fmt.Errorf("unexpected preparation")
	}
	if r.Preparation != nil && (len(r.Preparation.Summary) > 2048 || !utf8.ValidString(r.Preparation.Summary) || !safefs.DigestPattern.MatchString(r.Preparation.Hash)) {
		return receipt, fmt.Errorf("invalid preparation")
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if err := requireCurrentCommand(tx, r.CommandID); err != nil {
			return err
		}
		if err := s.requireUnretiredProducer(p); err != nil {
			return err
		}
		v, err := readCommand(tx, r.CommandID)
		if err != nil {
			return err
		}
		if v.Command.Producer != p || r.CommandHash != protocol.Hash(v.Command) || !v.Command.Scope.Allows(r.Scope) {
			return fmt.Errorf("result identity/scope mismatch")
		}
		var oldHash, oldReceipt string
		err = tx.QueryRow("SELECT hash,receipt FROM command_results WHERE command_id=? AND revision=?", r.CommandID, r.Revision).Scan(&oldHash, &oldReceipt)
		if err == nil {
			if oldHash != protocol.Hash(r) {
				return fmt.Errorf("result conflict")
			}
			return protocol.Decode([]byte(oldReceipt), &receipt, 8192)
		}
		if err != sql.ErrNoRows {
			return err
		}
		if r.Revision != v.Revision+1 {
			return fmt.Errorf("result revision gap: expected %d", v.Revision+1)
		}
		if err = expireCommand(tx, &v, s.now()); err != nil {
			return err
		}
		disposition := "applied"
		text := r.Text
		if interaction.Terminal(v.State) {
			if (v.State == "expired" || v.State == "rejected") && (v.Grant == nil || v.NonAdmission != nil) && ((r.Phase == "prepare" && r.State == "needs_confirmation") || r.State == "rejected") {
				disposition = "superseded_no_effect"
			} else {
				disposition = "execution_conflict"
				v.Conflict = true
			}
		} else if r.Phase == "prepare" && r.State == "rejected" {
			if v.State != "received" || !v.Command.Descriptor.Confirm || v.Grant != nil {
				return fmt.Errorf("invalid preparation rejection")
			}
			v.State = "rejected"
		} else if r.Phase == "prepare" {
			if v.State != "received" || !v.Command.Descriptor.Confirm || r.State != "needs_confirmation" || r.Preparation == nil || r.JobID != "" {
				return fmt.Errorf("invalid preparation transition")
			}
			expiry := r.Preparation.ExpiresAt
			if !expiry.After(s.now()) || expiry.After(s.now().Add(10*time.Minute)) || expiry.After(v.Command.Deadline) {
				return fmt.Errorf("invalid preparation deadline")
			}
			token, err := safefs.NewID("confirm")
			if err != nil {
				return err
			}
			prepared, _ := json.Marshal(r.Preparation)
			if _, err = tx.Exec("UPDATE commands SET preparation=?,token_hash=? WHERE id=?", string(prepared), protocol.Hash(token), r.CommandID); err != nil {
				return err
			}
			v.State = "needs_confirmation"
			text = r.Preparation.Summary + "\nconfirm " + token
		} else {
			if r.Preparation != nil {
				return fmt.Errorf("unexpected preparation")
			}
			if r.State == "rejected" && v.Grant == nil && (v.State == "queued" || v.State == "received") {
				v.State = "rejected"
			} else {
				if v.Grant == nil {
					return fmt.Errorf("execution without grant")
				}
				switch r.State {
				case "running":
					if v.State != "queued" && v.State != "outcome_unknown" {
						return fmt.Errorf("invalid running transition")
					}
				case "succeeded", "failed":
					if v.State != "queued" && v.State != "running" && v.State != "outcome_unknown" {
						return fmt.Errorf("invalid completion transition")
					}
				case "outcome_unknown":
					if v.State != "queued" && v.State != "running" {
						return fmt.Errorf("invalid uncertain transition")
					}
				default:
					return fmt.Errorf("unsupported result state")
				}
				v.State = r.State
			}
		}
		receipt = interaction.ResultReceipt{Identity: s.identity, Producer: p, CommandID: r.CommandID, Revision: r.Revision, Hash: protocol.Hash(r), Disposition: disposition}
		if interaction.Terminal(v.State) {
			receipt.TerminalState = v.State
			if v.Grant == nil || v.NonAdmission != nil {
				receipt.NonAdmission = true
				receipt.TerminalDecisionID = protocol.Hash([]string{v.Command.ID, v.State, v.Command.Deadline.UTC().Format(time.RFC3339Nano), "no-grant"})
				if v.NonAdmission != nil {
					receipt.TerminalDecisionID = v.NonAdmission.ID
				}
			}
		}
		body, _ := json.Marshal(r)
		ack, _ := json.Marshal(receipt)
		if err = s.capacity(tx, p.ID, len(body)+len(ack), false); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO command_results(command_id,revision,hash,body,receipt) VALUES(?,?,?,?,?)", r.CommandID, r.Revision, receipt.Hash, string(body), string(ack)); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE commands SET state=?,revision=?,conflict=? WHERE id=?", v.State, r.Revision, v.Conflict, r.CommandID); err != nil {
			return err
		}
		if interaction.Terminal(v.State) {
			if _, err = tx.Exec("UPDATE command_replies SET state='superseded' WHERE command_id=? AND state='pending'", r.CommandID); err != nil {
				return err
			}
		}
		if disposition == "applied" {
			return s.queueCommandReply(tx, v.Command, r.Revision, text, r.Scope)
		}
		return nil
	})
	return
}

func (s *Store) AckCommandResult(ctx context.Context, p protocol.Producer, receipt interaction.ResultReceipt) error {
	if receipt.Identity != s.identity || receipt.Producer != p {
		return fmt.Errorf("receipt identity mismatch")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := requireCurrentCommand(tx, receipt.CommandID); err != nil {
			return err
		}
		v, err := readCommand(tx, receipt.CommandID)
		if err != nil {
			return err
		}
		if v.Command.Producer != p {
			return fmt.Errorf("producer mismatch")
		}
		var body string
		if err = tx.QueryRow("SELECT receipt FROM command_results WHERE command_id=? AND revision=?", receipt.CommandID, receipt.Revision).Scan(&body); err != nil {
			return err
		}
		var stored interaction.ResultReceipt
		if err = protocol.Decode([]byte(body), &stored, 8192); err != nil {
			return err
		}
		if protocol.Hash(stored) != protocol.Hash(receipt) {
			return fmt.Errorf("receipt conflict")
		}
		_, err = tx.Exec("UPDATE command_results SET acked=1 WHERE command_id=? AND revision=?", receipt.CommandID, receipt.Revision)
		return err
	})
}

// PendingCommandReplies rechecks current audience rights; adapters must call it
// immediately before sending. No alert-route destination or fallback is used.
func (s *Store) PendingCommandReplies(ctx context.Context) (out []CommandReply, err error) {
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT body FROM command_replies WHERE state='pending' AND due<=? ORDER BY rowid LIMIT 32", s.now().UnixNano())
		if err != nil {
			return err
		}
		var replies []CommandReply
		for rows.Next() {
			var body string
			var r CommandReply
			if err = rows.Scan(&body); err == nil {
				err = protocol.Decode([]byte(body), &r, protocol.MaxRequest)
			}
			if err != nil {
				rows.Close()
				return err
			}
			replies = append(replies, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, r := range replies {
			if r.Kind == "gateway" {
				allowed, e := s.gatewayReplyAllowed(tx, r)
				if e != nil {
					return e
				}
				if allowed {
					out = append(out, r)
				} else {
					if _, e = tx.Exec("UPDATE command_replies SET state='blocked' WHERE command_id=? AND kind='gateway'", r.CommandID); e != nil {
						return e
					}
				}
				continue
			}
			v, err := readCommand(tx, r.CommandID)
			if err != nil {
				return err
			}
			if !s.commandAuthorized(v.Command, r.Scope) || v.Conflict {
				if _, err = tx.Exec("UPDATE command_replies SET state='blocked' WHERE command_id=? AND kind=? AND revision=?", r.CommandID, r.Kind, r.Revision); err != nil {
					return err
				}
				continue
			}
			out = append(out, r)
		}
		return nil
	})
	return
}
