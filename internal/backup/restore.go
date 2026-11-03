package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type ApplicationCheckResult struct {
	Schema             int    `json:"schema"`
	InputSHA256        string `json:"input_sha256"`
	ApplicationStarted bool   `json:"application_started"`
	QueryOK            bool   `json:"query_ok"`
	ApplicationVersion string `json:"application_version"`
	DatabaseSchema     string `json:"database_schema"`
}

type ApplicationVerification struct {
	Schema            int                         `json:"schema"`
	ID                string                      `json:"id"`
	ArchiveID         string                      `json:"archive_id"`
	BackupID          string                      `json:"backup_id"`
	SnapshotSHA256    string                      `json:"snapshot_sha256"`
	TestedInputSHA256 string                      `json:"tested_input_sha256"`
	Capture           protocol.SnapshotAttempt    `json:"capture"`
	Checker           config.WorkerRestoreCheck   `json:"checker"`
	ContractSHA256    string                      `json:"contract_sha256"`
	StartedAt         time.Time                   `json:"started_at"`
	FinishedAt        time.Time                   `json:"finished_at"`
	Structural        protocol.SnapshotValidation `json:"structural"`
	Application       ApplicationCheckResult      `json:"application"`
	NegativeControls  bool                        `json:"negative_controls"`
	Result            string                      `json:"result"`
}

type applicationCheckError struct {
	Kind  string
	Cause error
}

func (e *applicationCheckError) Error() string { return e.Kind + ": " + e.Cause.Error() }

func (e *applicationCheckError) Unwrap() error { return e.Cause }

func checkerRejected(err error) bool {
	var e *applicationCheckError
	return errors.As(err, &e) && e.Kind == "application-rejected"
}

type applicationChecker func(context.Context, string) (ApplicationCheckResult, error)

func restoreCheckContract(check config.WorkerRestoreCheck) string {
	body, _ := json.Marshal(check)
	return fmt.Sprintf("%x", sha256.Sum256(body))
}

func podmanApplicationChecker(check config.WorkerRestoreCheck) applicationChecker {
	return func(ctx context.Context, workspace string) (ApplicationCheckResult, error) {
		var result ApplicationCheckResult
		args := []string{"run", "--rm", fmt.Sprintf("--timeout=%d", int(config.Budget(check.Timeout, "2m").Seconds())), "--pull=never", "--network=none", "--read-only", "--cap-drop=all", "--security-opt=no-new-privileges", "--memory=512m", "--pids-limit=128", "--cpus=0.5", "--user=0:0", "--tmpfs=/tmp:rw,size=128m", "--volume", workspace + ":/restore:rw", "--env=DEPLOY_RESTORE_DATABASE=/restore/database.db", "--entrypoint", check.Command[0], check.Image}
		args = append(args, check.Command[1:]...)
		cmd := exec.Command("podman", args...)
		var out, stderr process.Buffer
		out.Limit = protocol.MaxBackupHeader
		stderr.Limit = 4096
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := process.RunContext(ctx, cmd, config.Budget(check.Timeout, "2m"), 10*time.Second); err != nil {
			kind := "execution-failed"
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() > 0 && exit.ExitCode() < 125 {
				kind = "application-rejected"
			}
			return result, &applicationCheckError{Kind: kind, Cause: fmt.Errorf("application checker did not complete successfully")}
		}
		if err := protocol.StrictBackupJSON(out.Bytes(), &result); err != nil {
			return result, err
		}
		return result, nil
	}
}

func applicationCheckSuite(ctx context.Context, base, positive string, expectedDigest string, checker applicationChecker) (ApplicationCheckResult, error) {
	var empty ApplicationCheckResult
	absent, err := os.MkdirTemp(base, "absent-")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(absent)
	if _, err = checker(ctx, absent); !checkerRejected(err) {
		return empty, fmt.Errorf("checker accepted an absent database; contract is ineffective")
	}
	incompatible, err := os.MkdirTemp(base, "incompatible-")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(incompatible)
	db, err := sql.Open("sqlite", filepath.Join(incompatible, "database.db"))
	if err != nil {
		return empty, err
	}
	_, err = db.Exec("CREATE TABLE deploy_negative_control (incompatible TEXT NOT NULL); INSERT INTO deploy_negative_control VALUES ('this is not an application database');")
	closeErr := db.Close()
	if err != nil {
		return empty, err
	}
	if closeErr != nil {
		return empty, closeErr
	}
	if _, err = checker(ctx, incompatible); !checkerRejected(err) {
		return empty, fmt.Errorf("checker accepted an incompatible database; contract is ineffective")
	}
	result, err := checker(ctx, positive)
	if err != nil {
		return result, err
	}
	if result.Schema != 1 || result.InputSHA256 != expectedDigest || !result.ApplicationStarted || !result.QueryOK || result.ApplicationVersion == "" || result.DatabaseSchema == "" {
		return result, fmt.Errorf("checker did not prove the selected input, application startup, and database queries")
	}
	return result, nil
}

func (a *backupArchive) restoreCheck(state *backupState, receipt SnapshotReceipt, check config.WorkerRestoreCheck) (ApplicationVerification, error) {
	record := ApplicationVerification{Schema: 1, ArchiveID: a.identity.ID, BackupID: receipt.BackupID, SnapshotSHA256: receipt.Header.SHA256, Capture: receipt.Header.Attempt, Checker: check, ContractSHA256: restoreCheckContract(check), StartedAt: time.Now().UTC(), Result: "inconclusive/execution-failed"}
	id, err := safefs.NewID("n")
	if err != nil {
		return record, err
	}
	record.ID = id
	structural, err := a.verify(state, receipt, true)
	record.Structural = structural.Validation
	if err != nil {
		return record, err
	}
	dir, lease, err := a.openBundle(receipt)
	if err != nil {
		return record, err
	}
	defer lease.Close()
	defer dir.Close()
	file, err := dir.Open("database.db", unix.O_RDONLY, 0)
	if err != nil {
		return record, err
	}
	defer file.Close()
	digest, err := safefs.DigestFile(file)
	if err != nil || digest != receipt.Header.SHA256 {
		return record, fmt.Errorf("restore input checksum failed")
	}
	if record.Structural.ForeignKeys != "ok" {
		return record, fmt.Errorf("foreign-key validation did not pass; structural integrity and application recovery are separate findings")
	}
	stateDir, err := safefs.Open(state.directory)
	if err != nil {
		return record, err
	}
	defer stateDir.Close()
	if err = stateDir.Capacity(2*receipt.Header.Size + 1<<30); err != nil {
		return record, err
	}
	base, err := os.MkdirTemp(state.directory, "restore-")
	if err != nil {
		return record, err
	}
	defer os.RemoveAll(base)
	workspace := filepath.Join(base, "positive")
	if err = os.Mkdir(workspace, 0700); err != nil {
		return record, err
	}
	clone, err := os.OpenFile(filepath.Join(workspace, "database.db"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return record, err
	}
	if _, err = file.Seek(0, io.SeekStart); err == nil {
		_, err = io.CopyN(clone, file, receipt.Header.Size)
	}
	if err == nil {
		err = clone.Sync()
	}
	if err == nil {
		record.TestedInputSHA256, err = safefs.DigestFile(clone)
	}
	sourceInfo, _ := file.Stat()
	cloneInfo, statErr := clone.Stat()
	clone.Close()
	if err != nil {
		return record, err
	}
	if statErr != nil || os.SameFile(sourceInfo, cloneInfo) || record.TestedInputSHA256 != receipt.Header.SHA256 {
		return record, fmt.Errorf("restore workspace is not an independent copy of the selected capture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*config.Budget(check.Timeout, "2m")+time.Minute)
	defer cancel()
	record.Application, err = applicationCheckSuite(ctx, base, workspace, receipt.Header.SHA256, podmanApplicationChecker(check))
	record.FinishedAt = time.Now().UTC()
	if checkerRejected(err) {
		record.Result = "validation-failed"
	}
	if err == nil {
		record.NegativeControls = true
		record.Result = "passed"
	}
	checkErr := err
	if err = a.updateControl(state, func(control *ArchiveControl) error {
		if err := a.dir.WriteJSON("control/verification/"+id+".json", record, false); err != nil {
			return err
		}
		if checkErr == nil {
			return advanceRecoveryPin(state, control, receipt, "application/"+instanceKey(receipt.Header.Attempt.SourceID, receipt.Header.Attempt.Service)+"/"+receipt.Header.Generation+"/"+record.ContractSHA256)
		}
		return nil
	}); err != nil {
		return record, err
	}
	return record, checkErr
}

func advanceRecoveryPin(state *backupState, control *ArchiveControl, receipt SnapshotReceipt, reason string) error {
	if control.Pins == nil {
		control.Pins = map[string]string{}
	}
	for id, reasons := range control.Pins {
		contains := false
		for _, value := range strings.Split(reasons, "\n") {
			contains = contains || value == reason
		}
		if !contains {
			continue
		}
		var raw string
		if err := state.db.QueryRow("SELECT record FROM bundles WHERE id=?", id).Scan(&raw); err != nil {
			return fmt.Errorf("existing recovery pin lacks catalog evidence")
		}
		var old SnapshotReceipt
		if err := safefs.DecodeJSON([]byte(raw), &old, safefs.MaxRecordBytes); err != nil {
			return err
		}
		if old.WriterEpoch > receipt.WriterEpoch || old.WriterEpoch == receipt.WriterEpoch && old.Header.Attempt.CaptureSequence > receipt.Header.Attempt.CaptureSequence {
			return nil
		}
	}
	for id, reasons := range control.Pins {
		var kept []string
		for _, value := range strings.Split(reasons, "\n") {
			if value != reason {
				kept = append(kept, value)
			}
		}
		if len(kept) == 0 {
			delete(control.Pins, id)
		} else {
			control.Pins[id] = strings.Join(kept, "\n")
		}
	}
	existing := control.Pins[receipt.BackupID]
	if existing != "" {
		existing += "\n"
	}
	control.Pins[receipt.BackupID] = existing + reason
	return nil
}
