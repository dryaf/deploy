package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"net/url"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

// RecoveryStatus inventories durable preparation, not authorization to publish,
// activate, execute or clear. Provider freshness and source/service handoffs must
// be checked again by the eventual publication coordinator under its own leases.
type RecoveryStatus struct {
	Version            int                   `json:"version"`
	Operation          string                `json:"operation"`
	Identity           protocol.Identity     `json:"identity"`
	ConfigHash         string                `json:"config_hash"`
	OriginalSourceHash string                `json:"original_source_hash"`
	ProjectionHash     string                `json:"projection_hash"`
	EvidenceHash       string                `json:"evidence_hash"`
	ReviewHash         string                `json:"review_hash"`
	Checks             []RecoveryStatusCheck `json:"checks"`
	PendingStages      int                   `json:"pending_stages"`
	PublicationAllowed bool                  `json:"publication_allowed"`
	DeliveryEnabled    bool                  `json:"delivery_enabled"`
	ExecutionEnabled   bool                  `json:"execution_enabled"`
}

type RecoveryStatusCheck struct {
	Scope     string `json:"scope"`
	Stage     string `json:"stage"`
	State     string `json:"state"` // complete, pending, retained_fenced
	ProofHash string `json:"proof_hash,omitempty"`
}

func (v *RecoveryStatus) add(scope, stage, state, proof string) {
	v.Checks = append(v.Checks, RecoveryStatusCheck{scope, stage, state, proof})
	if state == "pending" {
		v.PendingStages++
	}
}

// ReviewRecovery reads only an already initialized reconciliation database. It
// never initializes missing evidence or completes interrupted lifecycle intents.
func ReviewRecovery(ctx context.Context, path string, c Config, operation string) (out RecoveryStatus, err error) {
	if !safefs.ValidID(operation, "n") {
		return out, fmt.Errorf("exact recovery operation required")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	r, err := openRecoveryStatus(ctx, path, c, operation)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	return r.reviewRecovery(ctx)
}

func openRecoveryStatus(ctx context.Context, path string, c Config, operation string) (r *recoveryReconciler, err error) {
	return openRecoveryStatusMode(ctx, path, c, operation, false)
}

func openRecoveryStatusMode(ctx context.Context, path string, c Config, operation string, publishing bool) (r *recoveryReconciler, err error) {
	if err = c.Validate(); err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	r = &recoveryReconciler{config: c, now: time.Now, clock: process.HostClock}
	defer func() {
		if err != nil {
			r.Close()
		}
	}()
	if r.dir, err = privateDirectory(path); err != nil {
		return
	}
	if r.lease, err = r.dir.Lock("worker.lock", true, false); err != nil {
		return
	}
	if publishing {
		var saved RecoveryPublicationCommit
		err = readCheckpointJSON(r.dir, publicationCommitIntent, &saved)
		if err == nil {
			r.publication = &saved
		} else if !os.IsNotExist(err) {
			return
		}
	}
	if err = r.checkPublicationGate(); err != nil {
		return
	}
	if err = r.dir.ReadJSON("recovery.json", &r.journal); err != nil {
		return
	}
	if r.journal.Operation != operation || r.journal.Phase != "quarantined" {
		return r, fmt.Errorf("review requires the exact completed quarantine operation")
	}
	if err = finishSnapshotRecovery(ctx, r.dir, c, &r.journal, nil); err != nil {
		return
	}
	if r.stage, err = r.dir.Sub("recovery/" + operation); err != nil {
		return
	}
	info, e := r.stage.File().Stat()
	if e != nil {
		return r, e
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
		return r, fmt.Errorf("unsafe recovery stage directory")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		f, e := r.stage.Open("reconciled.db"+suffix, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return r, e
		}
		stat, e := f.Stat()
		f.Close()
		if e != nil {
			return r, e
		}
		owner, ok := stat.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Getuid()) || stat.Mode().Perm() != 0600 {
			return r, fmt.Errorf("unsafe reconciliation sidecar")
		}
	}
	var m reconciliationManifest
	if err = r.stage.ReadJSON("reconciliation.json", &m); err != nil {
		return
	}
	want := reconciliationManifest{1, r.journal.binding(), protocol.Hash(c), r.journal.CandidateHash, "ready"}
	if m != want {
		return r, fmt.Errorf("reconciliation preparation requires resume")
	}
	f, e := snapshotFile(r.stage, "reconciled.db", c.MaxDatabaseBytes)
	if e != nil {
		return r, e
	}
	f.Close()
	u := url.URL{Scheme: "file", Path: fmt.Sprintf("/proc/self/fd/%d/reconciled.db", r.stage.File().Fd())}
	u.RawQuery = url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(1500)", "query_only(ON)"}}.Encode()
	if r.db, err = sql.Open("sqlite", u.String()); err != nil {
		return
	}
	r.db.SetMaxOpenConns(1)
	want.State = "initializing"
	// A ready reconciliation must already have its SQL marker; initialize's
	// ready branch validates it and cannot create or repair any schema.
	if err = r.initialize(ctx, want, "ready"); err != nil {
		return
	}
	err = r.checkAuthority()
	return
}

func (r *recoveryReconciler) reviewRecovery(ctx context.Context) (out RecoveryStatus, err error) {
	err = r.withRecoveryReviewLeases(ctx, func(checks []func() error) error {
		out, err = r.reviewRecoveryLocked(ctx, checks)
		return err
	})
	return
}

// The mutex and every producer lease span both observation and any checkpoint
// made from it. A second review after releasing these leases is not equivalent.
func (r *recoveryReconciler) withRecoveryReviewLeases(ctx context.Context, action func([]func() error) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkReviewAuthority(ctx); err != nil {
		return err
	}
	enrollments := slices.Clone(r.config.Producers)
	slices.SortFunc(enrollments, func(a, b Enrollment) int {
		if a.Producer.ID < b.Producer.ID {
			return -1
		}
		if a.Producer.ID > b.Producer.ID {
			return 1
		}
		return 0
	})
	var checks []func() error
	var held []*safefs.Lock
	defer func() {
		for _, l := range held {
			l.Close()
		}
	}()
	for _, e := range enrollments {
		l, check, err := r.lockRecoveryProducer(ctx, e.Producer)
		if err != nil {
			return err
		}
		held = append(held, l)
		checks = append(checks, check)
	}
	if err := action(checks); err != nil {
		return err
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return r.checkReviewAuthority(ctx)
}

// Caller holds the recovery mutex, worker lease and all producer leases.
func (r *recoveryReconciler) reviewRecoveryLocked(ctx context.Context, checks []func() error) (out RecoveryStatus, err error) {
	if err = r.checkReviewAuthority(ctx); err != nil {
		return
	}
	for _, check := range checks {
		if err = check(); err != nil {
			return
		}
	}
	beforeFiles, err := r.recoveryEvidenceHash(ctx)
	if err != nil {
		return out, err
	}
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return out, err
	}
	defer conn.Close()
	var dataVersion int64
	if err = conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&dataVersion); err != nil {
		return out, err
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = r.checkRequestSeal(tx); err != nil {
		return out, err
	}
	out = RecoveryStatus{Version: 1, Operation: r.journal.Operation, Identity: r.journal.After, ConfigHash: protocol.Hash(r.config)}
	m, ack, exists, err := r.originalManifest(ctx, tx)
	if err != nil {
		return out, err
	}
	if exists {
		if err = r.checkStatusOriginal(m, ack); err != nil {
			return out, err
		}
	}
	if !exists || m.State != "ready" {
		out.add("worker", "original_source", "pending", "")
	} else {
		if ack != m.hash() {
			return out, fmt.Errorf("original source acknowledgement changed")
		}
		if err = absentOriginalTemporary(r.stage); err != nil {
			return out, err
		}
		if err = r.checkOriginalFiles(m, true); err != nil {
			return out, err
		}
		out.OriginalSourceHash = m.hash()
		out.add("worker", "original_source", "complete", m.hash())
	}
	if err = r.reviewRecoveryAdministration(ctx, tx, &out); err != nil {
		return out, err
	}
	for _, e := range r.config.Producers {
		if err = r.reviewRecoveryProducer(ctx, tx, e.Producer, &out); err != nil {
			return out, err
		}
	}
	if err = r.reviewRecoveryIntake(ctx, tx, &out); err != nil {
		return out, err
	}
	if err = r.reviewRecoveryLED(ctx, tx, &out); err != nil {
		return out, err
	}
	if out.ProjectionHash, err = recoveryProjectionHash(ctx, tx, r.config.MaxDatabaseBytes); err != nil {
		return out, err
	}
	if err = recoveryBoundary(r.fault, "recovery_status_observed"); err != nil {
		return out, err
	}
	for _, check := range checks {
		if err = check(); err != nil {
			return out, err
		}
	}
	if err = r.checkReviewAuthority(ctx); err != nil {
		return out, err
	}
	for _, e := range r.config.Producers {
		var current ownership
		if err = r.dir.ReadJSON(ownershipName(e.Producer)+".json", &current); err != nil {
			return out, err
		}
		s := &Store{dir: r.dir, identity: current.Identity, config: r.config}
		if err = s.verifyOwnershipPrefix(current); err != nil {
			return out, err
		}
	}
	if err = r.checkRequestSeal(tx); err != nil {
		return out, err
	}
	if exists {
		if err = r.checkStatusOriginal(m, ack); err != nil {
			return out, err
		}
	}
	afterFiles, err := r.recoveryEvidenceHash(ctx)
	if err != nil {
		return out, err
	}
	if beforeFiles != afterFiles {
		return out, fmt.Errorf("recovery evidence changed during review")
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	var currentVersion int64
	if err = conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&currentVersion); err != nil {
		return out, err
	}
	if currentVersion != dataVersion {
		return out, fmt.Errorf("recovery projection changed during review")
	}
	out.EvidenceHash = afterFiles
	slices.SortFunc(out.Checks, func(a, b RecoveryStatusCheck) int {
		if a.Scope < b.Scope {
			return -1
		}
		if a.Scope > b.Scope {
			return 1
		}
		if a.Stage < b.Stage {
			return -1
		}
		if a.Stage > b.Stage {
			return 1
		}
		return 0
	})
	out.ReviewHash = protocol.Hash(out)
	return out, nil
}

func (r *recoveryReconciler) checkReviewAuthority(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.checkAuthority(); err != nil {
		return err
	}
	if err := finishSnapshotRecovery(ctx, r.dir, r.config, &r.journal, nil); err != nil {
		return err
	}
	for _, name := range []string{"source.db", "candidate.db"} {
		if err := originalSidecars(r.stage, name, true); err != nil {
			return err
		}
	}
	return nil
}

func (r *recoveryReconciler) checkStatusOriginal(m recoveryOriginalManifest, ack string) error {
	if err := r.checkOriginalFiles(m, false); err != nil {
		return err
	}
	f, err := r.stage.Open(originalArchiveFile, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if os.IsNotExist(err) && ack == "" && m.State == "copying" {
		return nil
	}
	if err != nil {
		return err
	}
	f.Close()
	return r.checkOriginalFiles(m, true)
}
