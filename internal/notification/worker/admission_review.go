package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

// AdmissionCheck describes one prerequisite, never a permission to perform I/O.
// Reasons are fixed codes: database/provider errors and private inputs stay out
// of the review. A satisfied historical check still needs activation-time review.
type AdmissionCheck struct {
	Scope  string `json:"scope"`
	Stage  string `json:"stage"`
	State  string `json:"state"` // satisfied, blocked, not_required
	Reason string `json:"reason"`
}

type ManagedAdmissionReview struct {
	Identity   protocol.Identity                           `json:"identity"`
	CommitHash string                                      `json:"commit_hash,omitempty"`
	Checks     []AdmissionCheck                            `json:"checks"`
	Signal     map[string]interaction.SignalRecoveryCursor `json:"-"`
	Telegram   map[string]PublishedTelegramHandoff         `json:"-"`
}

// ReviewManagedAdmission reads initialized state under its exclusive worker
// lease. It does not open a Store, migrate a schema, repair evidence, poll a
// provider or change any runtime fence. The installation caller must separately
// retain its installation lease and verify service, profile and qualification
// authority. Returned observations cannot be used as an activation grant.
func ReviewManagedAdmission(ctx context.Context, path string, c Config, identity protocol.Identity, commit string, renewals map[string]string) (ManagedAdmissionReview, error) {
	return reviewManagedAdmission(ctx, path, c, identity, commit, renewals, telegramRenewalClock{time.Now, process.HostClock})
}

func reviewManagedAdmission(ctx context.Context, path string, c Config, identity protocol.Identity, commit string, renewals map[string]string, clock telegramRenewalClock) (out ManagedAdmissionReview, err error) {
	if err = c.Validate(); err != nil {
		return
	}
	if !identity.Valid() || commit != "" && !safefs.DigestPattern.MatchString(commit) || clock.now == nil || clock.host == nil {
		return out, fmt.Errorf("invalid managed admission pins or clock")
	}
	accounts := map[string]string{}
	if c.Interaction != nil {
		for _, d := range c.Interaction.Destinations {
			accounts[d.Provider+"/"+d.Account] = d.Provider
		}
	}
	for account, hash := range renewals {
		if commit == "" || accounts["telegram/"+account] != "telegram" || !safefs.DigestPattern.MatchString(hash) {
			return out, fmt.Errorf("invalid managed Telegram renewal reference")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	root, err := privateDirectory(path)
	if err != nil {
		return out, err
	}
	defer root.Close()
	lease, err := root.Lock("worker.lock", true, false)
	if err != nil {
		return out, err
	}
	defer lease.Close()
	live, err := snapshotFile(root, "state.db", c.MaxDatabaseBytes)
	if err != nil {
		return out, err
	}
	defer live.Close()
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := privateDirectory(path)
		if err != nil {
			return err
		}
		err = sameRetainedDirectory(root, current)
		current.Close()
		if err != nil {
			return err
		}
		for name, held := range map[string]*os.File{"worker.lock": lease.File(), "state.db": live} {
			f, err := root.Open(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			got, err := f.Stat()
			f.Close()
			if err != nil {
				return err
			}
			want, err := held.Stat()
			if err != nil {
				return err
			}
			if !os.SameFile(got, want) {
				return fmt.Errorf("managed admission coordination replaced")
			}
		}
		var got protocol.Identity
		if err := readCheckpointJSON(root, "identity.json", &got); err != nil {
			return err
		}
		if got != identity {
			return fmt.Errorf("managed admission identity changed")
		}
		for _, gate := range []string{"schema-migration.json", "led-migration.json"} {
			if err := absentOriginalFile(root, gate); err != nil {
				return err
			}
		}
		if err := verifyRecoveryRuntimeFiles(root, c); err != nil {
			return err
		}
		// immutable=1 avoids SQLite creating WAL/SHM coordination files during
		// review. It is safe only after proving no committed data is in a WAL
		// or hot rollback journal. Review never checkpoints or ignores such data.
		for _, suffix := range []string{"-wal", "-journal"} {
			f, err := root.Open("state.db"+suffix, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			info, err := f.Stat()
			f.Close()
			if err != nil {
				return err
			}
			if info.Size() != 0 {
				return fmt.Errorf("managed admission requires checkpointed SQLite state")
			}
		}
		return nil
	}
	if err = check(); err != nil {
		return out, err
	}
	db, err := immutableOriginalDB(root, "state.db")
	if err != nil {
		return out, err
	}
	defer db.Close()
	var version int
	var raw string
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return out, err
	}
	if version != currentSchema {
		return out, fmt.Errorf("managed admission requires the current schema")
	}
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&raw); err != nil {
		return out, err
	}
	var stored protocol.Identity
	if protocol.Decode([]byte(raw), &stored, 4096) != nil || stored != identity {
		return out, fmt.Errorf("managed admission database identity changed")
	}
	var enrolled int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM producers").Scan(&enrolled); err != nil || enrolled != len(c.Producers) {
		return out, fmt.Errorf("managed admission enrollment changed")
	}
	for _, enrollment := range c.Producers {
		var epoch string
		if err = db.QueryRowContext(ctx, "SELECT epoch FROM producers WHERE id=?", enrollment.Producer.ID).Scan(&epoch); err != nil || epoch != enrollment.Producer.Epoch {
			return out, fmt.Errorf("managed admission producer identity changed")
		}
	}
	out = ManagedAdmissionReview{Identity: identity, CommitHash: commit, Signal: map[string]interaction.SignalRecoveryCursor{}, Telegram: map[string]PublishedTelegramHandoff{}}
	add := func(scope, stage, state, reason string) {
		out.Checks = append(out.Checks, AdmissionCheck{scope, stage, state, reason})
	}
	if commit == "" {
		for _, name := range []string{"recovery.json", publicationCommitIntent, recoveryVerificationIntent, recoveryVerificationComplete} {
			if err = absentOriginalFile(root, name); err != nil {
				return ManagedAdmissionReview{}, err
			}
		}
		var count int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE key IN ('recovery',?)", publicationAnchorKey).Scan(&count); err != nil || count != 0 {
			return ManagedAdmissionReview{}, fmt.Errorf("managed admission has unbound recovery state")
		}
		add("recovery", "publication", "not_required", "initialized_state_without_recovery")
		for key := range accounts {
			add("intake/"+key, "handoff", "blocked", "initial_receiver_handoff_requires_activation_protocol")
		}
	} else {
		publication, proof, e := verifyCompletedPublication(ctx, root, lease, c, commit)
		if e != nil {
			return ManagedAdmissionReview{}, e
		}
		want := RecoveryVerification{1, commit, protocol.Hash(c), proof.Before, proof.After, true}
		if proof.After != identity {
			return ManagedAdmissionReview{}, fmt.Errorf("managed admission recovery identity changed")
		}
		for _, name := range []string{recoveryVerificationIntent, recoveryVerificationComplete} {
			ok, e := verificationRecord(root, name, want)
			if e != nil || !ok {
				return ManagedAdmissionReview{}, fmt.Errorf("managed admission recovery adoption incomplete")
			}
		}
		if err = verifyRecoveryRuntimeSQL(ctx, db, proof); err != nil {
			return ManagedAdmissionReview{}, err
		}
		add("recovery", "publication", "satisfied", "published_verification_remains_quarantined")
		stage, e := retainedDirectory(root, "recovery", publication.Image.Operation)
		if e != nil {
			return ManagedAdmissionReview{}, e
		}
		defer stage.Close()
		if c.Interaction != nil {
			tx, e := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if e != nil {
				return ManagedAdmissionReview{}, e
			}
			ready, e := requestRecoveryAdmissionReady(tx)
			if e == nil && ready {
				var journal snapshotRecovery
				e = readCheckpointJSON(root, "recovery.json", &journal)
				if e == nil {
					r := recoveryReconciler{dir: root, stage: stage, config: c, journal: journal}
					e = r.checkRequestSeal(tx)
				}
			}
			tx.Rollback()
			state, reason := "satisfied", "historical_requests_remain_fenced"
			if e != nil || !ready {
				state, reason = "blocked", "request_inventory_not_sealed"
			}
			add("commands", "request_seal", state, reason)
		}
		checkpoint, e := retainedDirectory(stage, "checkpoints", publication.Image.ReviewHash)
		if e != nil {
			return ManagedAdmissionReview{}, e
		}
		defer checkpoint.Close()
		retained, e := immutableOriginalDB(checkpoint, publicationDatabase)
		if e != nil {
			return ManagedAdmissionReview{}, e
		}
		defer retained.Close()
		keys := make([]string, 0, len(accounts))
		for key := range accounts {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			kind := accounts[key]
			account := key[len(kind)+1:]
			if kind == "signal" {
				original, e := readPublishedSignalCursor(ctx, retained, account, identity)
				actual, liveErr := readPublishedSignalCursor(ctx, db, account, identity)
				if e != nil || liveErr != nil || protocol.Hash(original) != protocol.Hash(actual) {
					add("intake/"+key, "cursor", "blocked", "signal_cursor_or_request_seal_changed")
				} else {
					out.Signal[account] = actual
					add("intake/"+key, "cursor", "satisfied", "published_signal_cursor_retained")
				}
			} else if kind == "telegram" {
				var handoff PublishedTelegramHandoff
				original, cursorErr := readPublishedTelegramCursor(ctx, retained, account)
				session := &publishedTelegramSession{root: root, stage: stage, lease: lease, borrowedLease: true, db: db, liveFile: live, config: c, publication: publication, proof: proof, cursor: original}
				if cursorErr != nil || original.Drain.Identity != identity || original.Drain.Operation != proof.Operation {
					e = fmt.Errorf("published Telegram cursor authority changed")
				} else if hash := renewals[account]; hash != "" {
					handoff, e = session.verifyRenewedHandoff(ctx, hash, clock)
				} else {
					handoff, e = session.verifyOriginalHandoff(ctx, clock.now, clock.host)
				}
				if e != nil {
					add("intake/"+key, "handoff", "blocked", "telegram_handoff_not_current")
				} else {
					out.Telegram[account] = handoff
					add("intake/"+key, "handoff", "satisfied", "telegram_handoff_current_recheck_required")
				}
			}
		}
		current, verified, e := verifyCompletedPublication(ctx, root, lease, c, commit)
		if e != nil || current != publication || verified != proof {
			return ManagedAdmissionReview{}, fmt.Errorf("managed admission publication changed during review")
		}
	}
	s := &Store{dir: root, config: c, identity: identity}
	for _, enrollment := range c.Producers {
		o, e := s.readOwnership(enrollment.Producer)
		if e != nil {
			return ManagedAdmissionReview{}, e
		}
		if o.Producer != enrollment.Producer || o.Identity.Worker != identity.Worker {
			return ManagedAdmissionReview{}, fmt.Errorf("managed admission ownership identity changed")
		}
		if err = s.checkOwnershipHistoryTail(enrollment.Producer, o.Revision); err != nil {
			return ManagedAdmissionReview{}, err
		}
		if err = s.verifyOwnershipPrefix(o); err != nil {
			return ManagedAdmissionReview{}, err
		}
		state, reason := "satisfied", "producer_generations_inactive"
		if o.Active != "" {
			state, reason = "blocked", "producer_generation_active_or_draining"
		}
		for _, generation := range o.Generations {
			if generation == "active" || generation == "draining" {
				state, reason = "blocked", "producer_generation_active_or_draining"
			}
		}
		add("producer/"+enrollment.Producer.ID, "inactive_generation", state, reason)
		add("producer/"+enrollment.Producer.ID, "activation", "blocked", "explicit_managed_producer_cutover_required")
	}
	if err = check(); err != nil {
		return ManagedAdmissionReview{}, err
	}
	slices.SortFunc(out.Checks, func(a, b AdmissionCheck) int { return strings.Compare(a.Scope+"/"+a.Stage, b.Scope+"/"+b.Stage) })
	return out, nil
}
