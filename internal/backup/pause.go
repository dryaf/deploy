package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type backupCatchup struct {
	ID          string    `json:"id"`
	RequestedAt time.Time `json:"requested_at"`
}

func schedulerLock(state *backupState) (*safefs.Lock, error) {
	return schedulerLockContext(context.Background(), state)
}

func schedulerLockContext(ctx context.Context, state *backupState) (*safefs.Lock, error) {
	// The notifier holds this lease across two HTTP requests, each bounded to
	// ten seconds. Ordinary contention must not discard an admitted backup or
	// its completed outcome. Bound the wait and retain pause/heartbeat ordering.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dir, err := safefs.Open(state.directory)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err = ctx.Err(); err != nil {
			return nil, fmt.Errorf("waiting for backup scheduler: %w", err)
		}
		lock, err := dir.Lock("scheduler.lock", true, true)
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for backup scheduler: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func putMetaTx(tx *sql.Tx, key string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(body))
	return err
}

func setWorkerPause(state *backupState, until time.Time) error {
	lock, err := schedulerLock(state)
	if err != nil {
		return err
	}
	defer lock.Close()
	tx, err := state.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = putMetaTx(tx, "paused_until", until.UTC()); err != nil {
		return err
	}
	if err = putMetaTx(tx, "backup_catchup", backupCatchup{}); err != nil {
		return err
	}
	return tx.Commit()
}

// Caller holds scheduler.lock. Clearing the pause and requesting a new capture
// are one transaction, so neither reboot nor a failed unit start loses the request.
func requestBackupCatchup(state *backupState, now time.Time, explicit bool) error {
	var paused time.Time
	if err := state.get("paused_until", &paused); err != nil && err != sql.ErrNoRows {
		return err
	}
	if paused.IsZero() || !explicit && now.Before(paused) {
		return nil
	}
	var suspect bool
	if err := state.get("clock_suspect", &suspect); err != nil && err != sql.ErrNoRows {
		return err
	}
	if suspect {
		return fmt.Errorf("clock uncertainty keeps backups paused; acknowledge the clock before resuming")
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return err
	}
	tx, err := state.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = putMetaTx(tx, "backup_catchup", backupCatchup{id, now.UTC()}); err != nil {
		return err
	}
	if err = putMetaTx(tx, "paused_until", time.Time{}); err != nil {
		return err
	}
	return tx.Commit()
}

func startBackupCatchup() error {
	_, err := process.LimitedOutput(exec.Command("systemctl", "--user", "start", "--no-block", "deploy-backup.service"), 10*time.Second, 4096)
	return err
}

// Only the host helper starts the fixed unit. The notifier has no host-control
// socket. Keep the request until a complete run commits; systemd and run.lock
// coalesce concurrent starts, including a lost start acknowledgement.
func consumeBackupCatchup(state *backupState, c WorkerInstallation, explicit bool, start func() error) error {
	// Most health probes have no scheduling work. Avoid waiting behind a
	// monitoring request merely to discover that there is nothing to resume.
	var paused time.Time
	if err := state.get("paused_until", &paused); err != nil && err != sql.ErrNoRows {
		return err
	}
	var pending backupCatchup
	if err := state.get("backup_catchup", &pending); err != nil && err != sql.ErrNoRows {
		return err
	}
	if pending.ID == "" && (paused.IsZero() || !explicit && time.Now().Before(paused)) {
		return nil
	}
	lock, err := schedulerLock(state)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = requestBackupCatchup(state, time.Now().UTC(), explicit); err != nil {
		return err
	}
	if err = state.get("backup_catchup", &pending); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	if pending.ID == "" {
		return nil
	}
	var mode string
	if err = state.get("mode", &mode); err != nil || mode != "writer" {
		return fmt.Errorf("catch-up awaits writer mode")
	}
	if err = state.resolveIncident("maintenance", "worker", time.Now().UTC(), c.Policy.Notifications); err != nil {
		return err
	}
	return start()
}

func admitWorkerRun(ctx context.Context, state *backupState, run WorkerRun) (string, error) {
	lock, err := schedulerLockContext(ctx, state)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err = requestBackupCatchup(state, time.Now().UTC(), false); err != nil {
		return "", err
	}
	var paused time.Time
	if err = state.get("paused_until", &paused); err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if !paused.IsZero() {
		return "", fmt.Errorf("worker is paused")
	}
	var pending backupCatchup
	if err = state.get("backup_catchup", &pending); err != nil && err != sql.ErrNoRows {
		return "", err
	}
	return pending.ID, state.put("run", run)
}

func finishWorkerRun(state *backupState, run WorkerRun, catchupID string) error {
	// Finalize even if the capture deadline elapsed; the bounded scheduler wait
	// has its own budget so a failed/timed-out run still records its outcome.
	lock, err := schedulerLock(state)
	if err != nil {
		return err
	}
	defer lock.Close()
	var pending backupCatchup
	if err = state.get("backup_catchup", &pending); err != nil && err != sql.ErrNoRows {
		return err
	}
	tx, err := state.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = putMetaTx(tx, "run", run); err != nil {
		return err
	}
	if run.Complete && catchupID != "" && pending.ID == catchupID {
		if err = putMetaTx(tx, "backup_catchup", backupCatchup{}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
