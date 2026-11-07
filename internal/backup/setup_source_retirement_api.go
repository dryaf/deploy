package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/transport"
)

func callRetainedSourceAdmin(ctx context.Context, target config.ServerConfig, request source.SourceEnrollmentRequest) (source.SourceEnrollmentResult, error) {
	var result source.SourceEnrollmentResult
	raw, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	cmd := transport.WorkerAdminShell(target, `exec "$HOME/.local/lib/deploy/backup-source/deploy" __source_admin`)
	cmd.Stdin = bytes.NewReader(raw)
	var output, errs process.Buffer
	output.Limit, errs.Limit = protocol.MaxBackupHeader, 8192
	cmd.Stdout, cmd.Stderr = &output, &errs
	if err = process.RunContext(ctx, cmd, 2*time.Minute, 5*time.Second); err != nil {
		return result, fmt.Errorf("source retirement interrupted; restore retained source access and retry the exact YAML")
	}
	if err = protocol.StrictBackupJSON(output.Bytes(), &result); err != nil {
		return result, fmt.Errorf("invalid source retirement response")
	}
	return result, nil
}

func retireSetupSources(ctx context.Context, request WorkerSetupRequest, administrators map[string]config.ServerConfig, workerID string, sources map[string]SetupSourceRetirement, revoke func(context.Context, config.ServerConfig, source.SourceEnrollmentRequest) (source.SourceEnrollmentResult, error)) (map[string]source.SourceEnrollmentResult, error) {
	if !safefs.ValidID(workerID, "w") || request.Declaration == nil || request.Declaration.Desired == nil || request.Declaration.Desired.Backup == nil || len(sources) == 0 || len(sources) > 128 {
		return nil, fmt.Errorf("source retirement requires exact YAML authority")
	}
	var aliases []string
	// Validate the whole batch before the first remote revocation. Addresses are
	// supplied by retained state, but authority to contact them comes from YAML.
	for alias, s := range sources {
		admin, ok := administrators[alias]
		port := admin.SSHPort
		if port == 0 {
			port = 22
		}
		if !ok || !config.ValidInventoryName(alias) || s.Target.Alias != alias || admin.Host != s.Target.Host || admin.User != s.Target.User || port != s.Target.Port || admin.User == "root" || !safefs.ValidID(s.SourceID, "s") || !safefs.DigestPattern.MatchString(s.HelperSHA256) || s.Fingerprint == "" || s.Reason == "" || request.Declaration.Desired.Backup.RetireSources[alias] != s.Reason || request.Declaration.Sources[alias] != "" {
			return nil, fmt.Errorf("source retirement differs from YAML or retained administrator inventory; restore the old source endpoint until retirement completes")
		}
		for _, active := range request.Sources {
			if active.Alias == alias {
				return nil, fmt.Errorf("cannot retire an active source")
			}
		}
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	results := map[string]source.SourceEnrollmentResult{}
	for _, alias := range aliases {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s := sources[alias]
		ack, err := revoke(ctx, administrators[alias], source.SourceEnrollmentRequest{Schema: 1, Operation: "revoke-retained", WorkerID: workerID, SourceID: s.SourceID, Previous: s.Fingerprint, HelperSHA256: s.HelperSHA256, Reason: s.Reason})
		if err != nil {
			return nil, err
		}
		if ack != (source.SourceEnrollmentResult{Schema: 1, SourceID: s.SourceID, WorkerID: workerID}) {
			return nil, fmt.Errorf("source retirement acknowledgement identity differs")
		}
		results[alias] = ack
	}
	return results, nil
}
