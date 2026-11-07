package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/transport"
)

type RemovalResult struct {
	WorkerID    string `json:"worker_id"`
	ArchiveID   string `json:"archive_id"`
	ArchivePath string `json:"archive_path"`
}

// UninstallWorker uses the specialist's retained stop/revoke/remove transaction.
// It never deletes the archive, account state or lifecycle history. Source keys
// must be revoked before owned installation files can be removed.
func UninstallWorker(ctx context.Context, target config.ServerConfig, sources map[string]config.ServerConfig, desiredHash string) (RemovalResult, error) {
	call := func(request workerLifecycleRequest) (workerLifecycleJournal, error) {
		var result workerLifecycleJournal
		raw, err := json.Marshal(request)
		if err != nil {
			return result, err
		}
		cmd := workerAdminCommand(target, []string{"__lifecycle"})
		cmd.Stdin = bytes.NewReader(raw)
		var output, errs process.Buffer
		output.Limit = safefs.MaxRecordBytes
		errs.Limit = 8192
		cmd.Stdout, cmd.Stderr = &output, &errs
		if err = process.RunContext(ctx, cmd, 5*time.Minute, 5*time.Second); err != nil {
			return result, fmt.Errorf("backup removal interrupted; repeat uninstall to reconcile the retained operation")
		}
		if err = safefs.DecodeJSON(output.Bytes(), &result, safefs.MaxRecordBytes); err != nil {
			return result, fmt.Errorf("invalid backup removal reply")
		}
		return result, nil
	}
	revoke := func(admin config.ServerConfig, request source.SourceEnrollmentRequest) (source.SourceEnrollmentResult, error) {
		var result source.SourceEnrollmentResult
		raw, err := json.Marshal(request)
		if err != nil {
			return result, err
		}
		cmd := transport.WorkerAdminShell(admin, `exec "$HOME/.local/lib/deploy/backup-source/deploy" __source_admin`)
		cmd.Stdin = bytes.NewReader(raw)
		var output, errs process.Buffer
		output.Limit = protocol.MaxBackupHeader
		errs.Limit = 8192
		cmd.Stdout, cmd.Stderr = &output, &errs
		if err = process.RunContext(ctx, cmd, 2*time.Minute, 5*time.Second); err != nil {
			return result, fmt.Errorf("source revocation interrupted; repeat uninstall to reconcile the retained operation")
		}
		if err = protocol.StrictBackupJSON(output.Bytes(), &result); err != nil {
			return result, fmt.Errorf("invalid source revocation reply")
		}
		return result, nil
	}
	return uninstallWorkerUsing(ctx, sources, desiredHash, call, revoke)
}

func uninstallWorkerUsing(ctx context.Context, sources map[string]config.ServerConfig, desiredHash string, call func(workerLifecycleRequest) (workerLifecycleJournal, error), revoke func(config.ServerConfig, source.SourceEnrollmentRequest) (source.SourceEnrollmentResult, error)) (RemovalResult, error) {
	var result RemovalResult
	if !safefs.DigestPattern.MatchString(desiredHash) {
		return result, fmt.Errorf("uninstall requires the declared policy pin")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	j, err := call(workerLifecycleRequest{Operation: "uninstall-prepare", DesiredHash: desiredHash})
	if err != nil {
		return result, err
	}
	if j.Operation != "uninstall" || (j.Schema != 1 && j.Schema != 2) || (j.Stage != "complete" && j.Stage != "prepared") || !safefs.ValidID(j.Installation.WorkerID, "w") || !safefs.ValidID(j.Installation.ArchiveID, "a") {
		return result, fmt.Errorf("invalid prepared removal authority")
	}
	result = RemovalResult{j.Installation.WorkerID, j.Installation.ArchiveID, j.Installation.Policy.Storage.Path}
	if j.Stage == "complete" {
		return result, nil
	}
	if j.NotificationOwner != nil || j.Installation.Policy.Notifications.Mode == "shared" {
		return result, fmt.Errorf("shared backup removal requires producer retirement through the domain workflow")
	}
	seen := map[string]bool{}
	for _, s := range j.Installation.Sources {
		admin, ok := sources[s.Alias]
		port := admin.SSHPort
		if port == 0 {
			port = 22
		}
		if !ok || seen[s.Alias] || admin.Host != s.Host || admin.User != s.User || port != s.Port || !safefs.ValidID(s.SourceID, "s") {
			return result, fmt.Errorf("source administrator inventory differs from retained removal; restore its endpoint before resuming")
		}
		seen[s.Alias] = true
	}
	var results []source.SourceEnrollmentResult
	for _, s := range j.Installation.Sources {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		ack, e := revoke(sources[s.Alias], source.SourceEnrollmentRequest{Schema: 1, Operation: "revoke", WorkerID: j.Installation.WorkerID, SourceID: s.SourceID})
		if e != nil {
			return result, e
		}
		if ack.Schema != 1 || ack.WorkerID != j.Installation.WorkerID || ack.SourceID != s.SourceID || ack.Fingerprint != "" {
			return result, fmt.Errorf("source revocation acknowledgement mismatch")
		}
		results = append(results, ack)
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	completed, err := call(workerLifecycleRequest{Operation: "uninstall-finish", DesiredHash: desiredHash, Results: results})
	if err != nil {
		return result, err
	}
	if completed.Stage != "complete" || completed.Operation != "uninstall" || completed.Schema != j.Schema || !reflect.DeepEqual(completed.Installation, j.Installation) {
		return result, fmt.Errorf("removal completion identity changed")
	}
	return result, nil
}
