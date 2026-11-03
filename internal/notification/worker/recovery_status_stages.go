package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func (r *recoveryReconciler) reviewRecoveryProducer(ctx context.Context, tx *sql.Tx, p protocol.Producer, out *RecoveryStatus) error {
	scope := "producer/" + p.ID
	var owner ownership
	if err := r.dir.ReadJSON(ownershipName(p)+".json", &owner); err != nil {
		return err
	}
	if !validOwnership(owner) || owner.Producer != p || (owner.Identity != r.journal.Before && owner.Identity != r.journal.After) {
		return fmt.Errorf("recovery ownership identity mismatch")
	}
	s := &Store{dir: r.dir, identity: owner.Identity, config: r.config}
	current, err := s.readOwnership(p)
	if err != nil {
		return err
	}
	if err = s.verifyOwnershipPrefix(current); err != nil {
		return err
	}
	if err = s.checkOwnershipHistoryTail(p, current.Revision); err != nil {
		return err
	}
	state := "pending"
	if current.Identity == r.journal.After {
		if current.Active != "" {
			return fmt.Errorf("recovered producer is unexpectedly active")
		}
		for _, v := range current.Generations {
			if v == "active" || v == "draining" {
				return fmt.Errorf("recovered producer has a dispatchable generation")
			}
		}
		if current.RecoveryPending != nil {
			if *current.RecoveryPending != r.journal.binding() {
				return fmt.Errorf("pending producer recovery changed")
			}
			state = "retained_fenced"
		} else {
			state = "complete"
		}
	}
	out.add(scope, "ownership", state, protocol.Hash(current))
	var raw, challenge, manifest string
	err = tx.QueryRowContext(ctx, "SELECT challenge,manifest,completion FROM reconciliation_producers WHERE producer=?", p.ID).Scan(&challenge, &manifest, &raw)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows || raw == "" {
		out.add(scope, "delivery_reconciliation", "pending", "")
	} else {
		summary, err := r.reviewRecoverySummary(ctx, tx, p, challenge, manifest, raw)
		if err != nil {
			return err
		}
		out.add(scope, "delivery_reconciliation", "complete", protocol.Hash(summary))
		if state == "complete" {
			if _, err = r.readRecoveryOwnership(p, protocol.Hash(summary)); err != nil {
				return err
			}
			var intent recoveryGenerationIntent
			err = r.stage.ReadJSON(generationFile(p), &intent)
			if os.IsNotExist(err) {
				out.add(scope, "generation", "pending", "")
			} else if err != nil {
				return err
			} else {
				g, err := r.readRecoveryGeneration(p, protocol.Hash(summary))
				if err != nil {
					return err
				}
				out.add(scope, "generation", "complete", protocol.Hash(g))
			}
		}
	}
	interactive := false
	if r.config.Interaction != nil {
		for _, w := range r.config.Interaction.Workers {
			if w.Producer == p {
				interactive = true
			}
		}
	}
	if !interactive {
		if err = requireRecoveryCommandsAbsent(tx, p); err != nil {
			return err
		}
		out.add(scope, "commands", "complete", protocol.Hash("no-command-history"))
	} else if err = r.reviewRecoveryCommands(tx, p, out); err != nil {
		return err
	}
	return r.reviewRecoveryMessages(ctx, tx, p, out)
}

func (r *recoveryReconciler) reviewRecoverySummary(ctx context.Context, tx *sql.Tx, p protocol.Producer, chRaw, mRaw, raw string) (v protocol.RecoverySummary, err error) {
	var ch protocol.RecoveryChallenge
	var m protocol.RecoveryManifest
	if err = protocol.Decode([]byte(raw), &v, protocol.MaxRequest+8192); err != nil {
		return
	}
	if err = protocol.Decode([]byte(chRaw), &ch, 4096); err != nil {
		return
	}
	if err = protocol.Decode([]byte(mRaw), &m, 4096); err != nil {
		return
	}
	seq := func(n uint64) bool { return n == 0 || protocol.Sequence(n) }
	if ch.Operation != r.journal.Operation || ch.Identity != r.journal.After || ch.Producer != p || !safefs.ValidID(ch.Nonce, "n") || ch.IssuedAt.IsZero() || !ch.ExpiresAt.After(ch.IssuedAt) || v.Challenge != ch || v.Manifest != m || v.DeliveryEnabled || v.Snapshot.Producer != p || v.Ambiguous < 0 || !seq(m.HighestIssued) || !seq(m.HighestAcknowledged) || !seq(m.CompactedThrough) || m.CompactedThrough > m.HighestAcknowledged || m.HighestAcknowledged > m.HighestIssued || m.ImportedReceipts.Identity != r.journal.Before || m.ImportedReceipts.Producer != p || m.ImportedReceipts.Position < 0 || (m.CompactedThrough > 0 && !safefs.DigestPattern.MatchString(m.OutcomeCheckpoint)) || (m.CompactedThrough == 0 && m.OutcomeCheckpoint != "") {
		return v, fmt.Errorf("recovery summary authority changed")
	}
	if err = v.Snapshot.Validate(); err != nil {
		return
	}
	var submitted, oldFloor uint64
	var epoch string
	if err = tx.QueryRowContext(ctx, "SELECT epoch,submitted FROM producers WHERE id=?", p.ID).Scan(&epoch, &submitted); err != nil {
		return
	}
	if err = tx.QueryRowContext(ctx, "SELECT submitted FROM recovery_producers WHERE id=?", p.ID).Scan(&oldFloor); err != nil {
		return
	}
	if epoch != p.Epoch || submitted != m.HighestIssued || oldFloor > m.HighestIssued {
		return v, fmt.Errorf("recovery producer watermarks changed")
	}
	first, last := uint64(0), uint64(0)
	if m.CompactedThrough > oldFloor {
		first, last = oldFloor+1, m.CompactedThrough
	}
	if v.UnknownFirst != first || v.UnknownLast != last {
		return v, fmt.Errorf("recovery compacted gap changed")
	}
	if first != 0 {
		var a, b uint64
		var checkpoint string
		if err = tx.QueryRowContext(ctx, "SELECT first,last,checkpoint FROM reconciliation_unknown WHERE producer=?", p.ID).Scan(&a, &b, &checkpoint); err != nil {
			return
		}
		if a != first || b != last || checkpoint != m.OutcomeCheckpoint {
			return v, fmt.Errorf("recovery compacted-gap checkpoint changed")
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT seq,id,hash,evidence_hash,body FROM reconciliation_records WHERE producer=? ORDER BY seq", p.ID)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	n := m.CompactedThrough
	for rows.Next() {
		var seq uint64
		var id, hash, evidence, body string
		if err = rows.Scan(&seq, &id, &hash, &evidence, &body); err != nil {
			return
		}
		var record protocol.RecoveryRecord
		if err = protocol.Decode([]byte(body), &record, protocol.MaxRequest); err != nil {
			return
		}
		if err = record.Message.Validate(); err != nil {
			return
		}
		n++
		if seq != n || seq > m.HighestIssued || record.Message.Sequence != seq || record.Message.ID != id || record.Message.Producer != p || protocol.Hash(record) != evidence || protocol.Hash(record.Message) != hash {
			return v, fmt.Errorf("recovery evidence membership changed")
		}
		var projectedHash, projectedBody string
		var projectedSeq uint64
		if err = tx.QueryRowContext(ctx, "SELECT hash,seq,body FROM messages WHERE producer=? AND id=?", p.ID, id).Scan(&projectedHash, &projectedSeq, &projectedBody); err != nil {
			return
		}
		var projected protocol.Message
		if err = protocol.Decode([]byte(projectedBody), &projected, protocol.MaxRequest); err != nil {
			return
		}
		if projectedHash != hash || projectedSeq != seq || protocol.Hash(projected) != hash {
			return v, fmt.Errorf("recovered message projection changed")
		}
		if retained := record.Receipt; retained != nil {
			receipt := retained.Receipt
			if retained.Identity != r.journal.Before || retained.Producer != p || receipt.EventID != id || receipt.Sequence != seq || receipt.Position <= 0 || receipt.Position > m.ImportedReceipts.Position || !terminal(receipt.State) || receipt.At.IsZero() {
				return v, fmt.Errorf("retained receipt provenance changed")
			}
			var receiptHash, receiptRaw, receiptProducer string
			if err = tx.QueryRowContext(ctx, "SELECT producer,hash,body FROM reconciliation_receipts WHERE position=?", receipt.Position).Scan(&receiptProducer, &receiptHash, &receiptRaw); err != nil {
				return
			}
			var saved protocol.RetainedReceipt
			if err = protocol.Decode([]byte(receiptRaw), &saved, 8192); err != nil {
				return
			}
			if receiptProducer != p.ID || receiptHash != protocol.Hash(*retained) || protocol.Hash(saved) != receiptHash {
				return v, fmt.Errorf("retained receipt evidence changed")
			}
		}
	}
	if err = rows.Err(); err != nil {
		return
	}
	if n != m.HighestIssued {
		return v, fmt.Errorf("recovery evidence missing submissions")
	}
	return
}

func (r *recoveryReconciler) reviewRecoveryCommands(tx *sql.Tx, p protocol.Producer, out *RecoveryStatus) error {
	scope := "producer/" + p.ID
	incomplete := func() error {
		exists, err := commandInstallationSchema(tx)
		if err != nil {
			return err
		}
		if exists {
			var count int
			if err = tx.QueryRow("SELECT count(*) FROM reconciliation_command_installations WHERE producer=?", p.ID).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("installed command outcomes lost their reconciliation")
			}
		}
		if err = absentOriginalFile(r.stage, commandInstallationFile(p)); err != nil {
			return err
		}
		out.add(scope, "commands", "pending", "")
		return nil
	}
	var markers, tables int
	if err := tx.QueryRow("SELECT count(*) FROM metadata WHERE key='command_reconciliation_schema'").Scan(&markers); err != nil {
		return err
	}
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('reconciliation_command_sessions','reconciliation_command_records')").Scan(&tables); err != nil {
		return err
	}
	if markers == 0 && tables == 0 {
		return incomplete()
	}
	var version string
	if markers != 1 || tables != 2 {
		return fmt.Errorf("command recovery schema changed")
	}
	if err := tx.QueryRow("SELECT value FROM metadata WHERE key='command_reconciliation_schema'").Scan(&version); err != nil {
		return err
	}
	if version != "1" {
		return fmt.Errorf("unsupported command recovery schema")
	}
	session, err := readCommandRecoverySession(tx, p)
	if err == sql.ErrNoRows {
		return incomplete()
	}
	if err != nil {
		return err
	}
	if session.Summary == nil || session.Manifest == nil {
		return incomplete()
	}
	var checkpoint interaction.CommandRecoveryCommit
	if err = r.prepareCommandRecoveryCommit(tx, p, &checkpoint); err != nil {
		return err
	}
	installed, err := commandInstallationSchema(tx)
	if err != nil {
		return err
	}
	if !installed {
		if err = absentOriginalFile(r.stage, commandInstallationFile(p)); err != nil {
			return err
		}
		out.add(scope, "commands", "pending", protocol.Hash(checkpoint))
		return nil
	}
	var raw, hash string
	err = tx.QueryRow("SELECT body,hash FROM reconciliation_command_installations WHERE producer=?", p.ID).Scan(&raw, &hash)
	if err == sql.ErrNoRows {
		plan, projection, e := commandInstallationPlan(tx, checkpoint)
		if e != nil {
			return e
		}
		var intent commandInstallationIntent
		fileErr := r.stage.ReadJSON(commandInstallationFile(p), &intent)
		if fileErr != nil && !os.IsNotExist(fileErr) {
			return fileErr
		}
		if fileErr == nil && protocol.Hash(plan) != protocol.Hash(intent) {
			return fmt.Errorf("pending command installation intent changed")
		}
		if projection != plan.BeforeHash {
			return fmt.Errorf("unacknowledged command installation projection changed")
		}
		out.add(scope, "commands", "pending", protocol.Hash(checkpoint))
		return nil
	}
	if err != nil {
		return err
	}
	var result interaction.CommandRecoveryInstallation
	if err = protocol.Decode([]byte(raw), &result, 32768); err != nil {
		return err
	}
	if err = result.Validate(); err != nil {
		return err
	}
	plan, projection, err := commandInstallationPlan(tx, checkpoint)
	if err != nil {
		return err
	}
	var intent commandInstallationIntent
	if err = r.stage.ReadJSON(commandInstallationFile(p), &intent); err != nil {
		return err
	}
	if protocol.Hash(plan) != protocol.Hash(intent) || protocol.Hash(plan.Installation) != hash || protocol.Hash(result) != hash || projection != result.OutcomesHash {
		return fmt.Errorf("command recovery installation changed")
	}
	out.add(scope, "commands", "complete", hash)
	return nil
}

func (r *recoveryReconciler) reviewRecoveryMessages(ctx context.Context, tx *sql.Tx, p protocol.Producer, out *RecoveryStatus) error {
	rows, err := tx.QueryContext(ctx, "SELECT id,state,attempt,hash,body,seq FROM messages WHERE producer=? ORDER BY id LIMIT ?", p.ID, r.config.MaxMessages+1)
	if err != nil {
		return err
	}
	type head struct{ ID, State, Attempt string }
	var heads []head
	for rows.Next() {
		var v head
		var hash, raw string
		var seq uint64
		if err = rows.Scan(&v.ID, &v.State, &v.Attempt, &hash, &raw, &seq); err != nil {
			break
		}
		var message protocol.Message
		if err = protocol.Decode([]byte(raw), &message, protocol.MaxRequest); err != nil {
			break
		}
		if err = message.Validate(); err != nil {
			break
		}
		if message.ID != v.ID || message.Producer != p || message.Sequence != seq || protocol.Hash(message) != hash {
			err = fmt.Errorf("message projection identity changed")
			break
		}
		heads = append(heads, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if len(heads) > r.config.MaxMessages {
		return fmt.Errorf("recovery message inventory exceeds budget")
	}
	var dispositionTables, dispositionMarkers int
	if err = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='reconciliation_dispositions'").Scan(&dispositionTables); err != nil {
		return err
	}
	if err = tx.QueryRow("SELECT count(*) FROM metadata WHERE key='recovery_dispositions_schema'").Scan(&dispositionMarkers); err != nil {
		return err
	}
	if dispositionTables != dispositionMarkers {
		return fmt.Errorf("disposition schema lost its marker or table")
	}
	if dispositionMarkers == 1 {
		var version string
		if err = tx.QueryRow("SELECT value FROM metadata WHERE key='recovery_dispositions_schema'").Scan(&version); err != nil {
			return err
		}
		if version != "1" {
			return fmt.Errorf("unsupported disposition schema")
		}
	}
	state := "complete"
	for _, v := range heads {
		decisionPhase := "none"
		if dispositionTables == 1 {
			_, decisionPhase, err = r.dispositionEvidence(tx, p, v.ID)
			if err != nil {
				return err
			}
		} else if err = absentOriginalFile(r.stage, dispositionFile(p, v.ID)); err != nil {
			return err
		}
		_, applicationPhase, e := r.applicationEvidence(tx, p, v.ID)
		if e != nil {
			return e
		}
		if decisionPhase == "intent" || (decisionPhase == "recorded" && applicationPhase != "applied") || applicationPhase == "intent" {
			state = "pending"
		}
		if v.Attempt != "" || !slices.Contains([]string{"ambiguous", "provider_accepted", "cancelled", "superseded", "expired", "fenced"}, v.State) {
			return fmt.Errorf("unexpected runnable recovery message")
		}
		if v.State == "ambiguous" {
			if state == "complete" {
				state = "retained_fenced"
			}
			continue
		}
		if v.State == "fenced" {
			_, phase, err := r.retryBindingEvidence(tx, p, v.ID)
			if err != nil {
				return err
			}
			if phase != "bound" {
				state = "pending"
			}
		}
	}
	out.add("producer/"+p.ID, "message_dispositions", state, protocol.Hash(heads))
	return nil
}
