package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/crypto/ssh"
)

// SetupArtifact is an operator-local helper selected for a particular host.
// The digest is verified before any mutation and again immediately before send.
type SetupArtifact struct {
	File, SHA256, Architecture string
}

type SetupTarget struct {
	Server config.ServerConfig
	Helper SetupArtifact
}

// SetupPlan contains resolved, non-secret inputs. Source host keys must already
// have been verified by the operator. Runtime identities come from the target's
// retained setup journal, never from the caller or a regenerated configuration.
type SetupPlan struct {
	SourceAdministrators map[string]config.ServerConfig
	Target               SetupTarget
	Sources              map[string]SetupTarget
	Request              WorkerSetupRequest
}

// SetupWorker exposes the existing installation transaction without CLI parsing
// or global UI state. A lost reply is recovered by repeating this exact plan.
// It stages an installation; qualification and activation remain explicit.
func SetupWorker(ctx context.Context, plan SetupPlan) (WorkerSetupResponse, error) {
	var result WorkerSetupResponse
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := validateSetupPlan(plan); err != nil {
		return result, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, binary, err := bootstrapHelperCommand(plan.Target.Server, plan.Target.Helper.File, "__worker_setup")
	if err != nil {
		return result, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(binary)) != plan.Target.Helper.SHA256 {
		return result, fmt.Errorf("worker helper changed before bootstrap")
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return result, err
	}
	defer in.Close()
	out, writer, err := os.Pipe()
	if err != nil {
		return result, err
	}
	defer out.Close()
	cmd.Stdout = writer
	var stderr process.Buffer
	stderr.Limit = 8192
	cmd.Stderr = &stderr
	done := make(chan error, 1)
	go func() { defer writer.Close(); done <- process.RunContext(ctx, cmd, time.Hour, 2*time.Second) }()
	finished := false
	defer func() {
		in.Close()
		if !finished {
			cancel()
			out.Close()
			<-done
		}
	}()
	if _, err = in.Write(binary); err != nil {
		return result, fmt.Errorf("worker bootstrap interrupted; retry the same plan")
	}
	enroll := func(s SetupSource, workerID, key string) (deploysource.SourceEnrollmentResult, error) {
		return callSetupSourceAdmin(ctx, plan.Sources[s.Alias], deploysource.SourceEnrollmentRequest{Schema: 1, Operation: "enroll", WorkerID: workerID, PublicKey: key})
	}
	inspect := func(s SetupSource, workerID, key string, retained deploysource.SourceEnrollmentResult) (deploysource.SourceEnrollmentResult, error) {
		if plan.Request.Declaration == nil {
			return deploysource.SourceEnrollmentResult{}, fmt.Errorf("source inspection requires a declarative artifact pin")
		}
		return callSetupSourceAdmin(ctx, plan.Sources[s.Alias], deploysource.SourceEnrollmentRequest{Schema: 1, Operation: "inspect", WorkerID: workerID, SourceID: retained.SourceID, PublicKey: key, HelperSHA256: plan.Request.Declaration.Sources[s.Alias]})
	}
	retire := func(workerID string, sources map[string]SetupSourceRetirement) (map[string]deploysource.SourceEnrollmentResult, error) {
		return retireSetupSources(ctx, plan.Request, plan.SourceAdministrators, workerID, sources, callRetainedSourceAdmin)
	}
	result, err = exchangeWorkerSetupWithRetirement(plan.Request, out, in, enroll, inspect, retire)
	if err != nil {
		return result, err
	}
	in.Close()
	err = <-done
	finished = true
	if err != nil {
		return result, fmt.Errorf("worker completion reply was not confirmed; retry the same plan")
	}
	return result, nil
}

func callSetupSourceAdmin(ctx context.Context, target SetupTarget, request deploysource.SourceEnrollmentRequest) (deploysource.SourceEnrollmentResult, error) {
	var result deploysource.SourceEnrollmentResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	command, helper, err := bootstrapHelperCommand(target.Server, target.Helper.File, "__source_admin")
	if err != nil {
		return result, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(helper)) != target.Helper.SHA256 {
		return result, fmt.Errorf("source helper changed before bootstrap")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	command.Stdin = io.MultiReader(bytes.NewReader(helper), bytes.NewReader(body))
	var output, errs process.Buffer
	output.Limit, errs.Limit = protocol.MaxBackupHeader, 8192
	command.Stdout, command.Stderr = &output, &errs
	if err = process.RunContext(ctx, command, 2*time.Minute, 2*time.Second); err != nil {
		return result, fmt.Errorf("source enrollment or retained-identity inspection interrupted; setup journal retained for retry")
	}
	if err = protocol.StrictBackupJSON(output.Bytes(), &result); err != nil {
		return result, fmt.Errorf("invalid source enrollment or inspection response")
	}
	return result, nil
}

func validateSetupPlan(p SetupPlan) error {
	if p.Request.Schema != 1 || !validSetupOperation(p.Request) || p.Request.Recover && !p.Request.Fenced {
		return fmt.Errorf("invalid setup request")
	}
	policy, err := config.NormalizeWorker(p.Request.Policy)
	if err != nil {
		return err
	}
	if d := p.Request.Declaration; d != nil {
		if err := validateSetupDesired(d); err != nil {
			return err
		}
		if (d.Version != 1 && d.Version != 2) || !safefs.DigestPattern.MatchString(d.DesiredHash) || d.HelperSHA256 != p.Target.Helper.SHA256 || d.Architecture != p.Target.Helper.Architecture || d.User != p.Target.Server.User || d.UID <= 0 || !config.WorkerAbsolutePath(d.Home) || len(d.Sources) != len(p.Sources) {
			return fmt.Errorf("declarative setup pins differ from transport plan")
		}
		for alias, source := range p.Sources {
			if d.Sources[alias] != source.Helper.SHA256 {
				return fmt.Errorf("source helper differs from declarative pin")
			}
		}
	}
	if err = config.PinnedWorkerImage(p.Request.Image); err != nil {
		return err
	}
	if policy.Notifications.Signal != nil {
		if err = config.PinnedWorkerImage(p.Request.RelayImage); err != nil {
			return err
		}
	}
	if len(p.Sources) != len(policy.Sources) || len(p.Request.Sources) != len(policy.Sources) {
		return fmt.Errorf("setup source set differs from policy")
	}
	seen := map[string]bool{}
	for _, s := range p.Request.Sources {
		target, ok := p.Sources[s.Alias]
		port := target.Server.SSHPort
		if port == 0 {
			port = 22
		}
		if !ok || seen[s.Alias] || target.Server.Host != s.Host || target.Server.User != s.User || port != s.Port || len(s.KnownHosts) == 0 || len(s.KnownHosts) > 16384 {
			return fmt.Errorf("source endpoint or trust does not match setup plan")
		}
		workerPort := p.Target.Server.SSHPort
		if workerPort == 0 {
			workerPort = 22
		}
		if target.Server.Host == p.Target.Server.Host && target.Server.User == p.Target.Server.User && s.Port == workerPort {
			return fmt.Errorf("worker and source require distinct accounts")
		}
		seen[s.Alias] = true
	}
	for _, alias := range policy.Sources {
		if !seen[alias] {
			return fmt.Errorf("setup source set differs from policy")
		}
	}
	targets := []SetupTarget{p.Target}
	for _, s := range p.Sources {
		targets = append(targets, s)
	}
	for _, target := range targets {
		if target.Server.User == "root" || !config.ValidInventoryName(target.Server.User) || target.Server.Host == "" {
			return fmt.Errorf("setup requires rootless targets")
		}
		if err := verifySetupArtifact(target.Helper); err != nil {
			return err
		}
	}
	return nil
}

func verifySetupArtifact(a SetupArtifact) error {
	if !safefs.DigestPattern.MatchString(a.SHA256) {
		return fmt.Errorf("setup helper requires a SHA256 digest")
	}
	f, err := os.Open(a.File)
	if err != nil {
		return fmt.Errorf("setup helper unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 128<<20 {
		return fmt.Errorf("invalid setup helper size or type")
	}
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(f, 128<<20+1)); err != nil || fmt.Sprintf("%x", h.Sum(nil)) != a.SHA256 {
		return fmt.Errorf("setup helper digest mismatch")
	}
	binary, err := elf.NewFile(f)
	if err != nil {
		return fmt.Errorf("setup helper must be a static Linux ELF executable")
	}
	defer binary.Close()
	expected := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[a.Architecture]
	if expected == 0 || binary.Machine != expected || binary.Class != elf.ELFCLASS64 || binary.Data != elf.ELFDATA2LSB || (binary.Type != elf.ET_EXEC && binary.Type != elf.ET_DYN) {
		return fmt.Errorf("setup helper architecture mismatch")
	}
	entry := false
	for _, p := range binary.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("setup helper must be statically linked")
		}
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 && binary.Entry >= p.Vaddr && binary.Entry-p.Vaddr < p.Filesz {
			entry = true
		}
	}
	if !entry {
		return fmt.Errorf("setup helper has no executable entry point")
	}
	return nil
}

type enrollSetupSource func(SetupSource, string, string) (deploysource.SourceEnrollmentResult, error)
type inspectSetupSource func(SetupSource, string, string, deploysource.SourceEnrollmentResult) (deploysource.SourceEnrollmentResult, error)

// exchangeWorkerSetup validates the complete response chain, including stable
// archive/worker identities. No source enrollment happens for a completed retry.
func exchangeWorkerSetup(request WorkerSetupRequest, out io.Reader, in io.Writer, enroll enrollSetupSource, inspections ...inspectSetupSource) (WorkerSetupResponse, error) {
	var inspect inspectSetupSource
	if len(inspections) == 1 {
		inspect = inspections[0]
	}
	return exchangeWorkerSetupWithRetirement(request, out, in, enroll, inspect, nil)
}

type retireSetupSourceBatch func(string, map[string]SetupSourceRetirement) (map[string]deploysource.SourceEnrollmentResult, error)

func exchangeWorkerSetupWithRetirement(request WorkerSetupRequest, out io.Reader, in io.Writer, enroll enrollSetupSource, inspect inspectSetupSource, retire retireSetupSourceBatch) (WorkerSetupResponse, error) {
	var last WorkerSetupResponse
	write := func(v any) error {
		body, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return protocol.WriteBackupFrame(in, body)
	}
	read := func() (WorkerSetupResponse, error) {
		var response WorkerSetupResponse
		raw, err := protocol.ReadBackupFrame(out)
		if err != nil {
			return response, fmt.Errorf("setup coordinator reply unavailable; retry the same plan")
		}
		if err = protocol.StrictBackupJSON(raw, &response); err != nil || response.Error != "" {
			return response, fmt.Errorf("setup coordinator rejected the operation; inspect retained target setup state")
		}
		if response.Stage != "retiring-sources" && len(response.RetiringSources) != 0 {
			return response, fmt.Errorf("unexpected source retirement authority")
		}
		if response.Stage != "prepared" && len(response.ReadOnlySources) != 0 {
			return response, fmt.Errorf("unexpected source inspection authority")
		}
		if response.Schema == 1 && response.Stage == "source-recorded" && response.WorkerID == "" && response.ArchiveID == "" {
			return response, nil
		}
		if response.Schema != 1 || !safefs.ValidID(response.WorkerID, "w") || !safefs.ValidID(response.ArchiveID, "a") {
			return response, fmt.Errorf("invalid setup coordinator identity")
		}
		if last.WorkerID != "" && (last.WorkerID != response.WorkerID || last.ArchiveID != response.ArchiveID) {
			return response, fmt.Errorf("setup coordinator identity changed")
		}
		last = response
		return response, nil
	}
	finishRetirement := func(response WorkerSetupResponse) (WorkerSetupResponse, error) {
		if retire == nil || len(response.RetiringSources) == 0 || len(response.PublicKeys) != 0 || response.Producer != nil || response.PolicyUpdateRequired || response.Unchanged {
			return last, fmt.Errorf("invalid source retirement response")
		}
		acks, err := retire(response.WorkerID, response.RetiringSources)
		if err != nil {
			return last, err
		}
		if len(acks) != len(response.RetiringSources) {
			return last, fmt.Errorf("incomplete source retirement results")
		}
		for alias, s := range response.RetiringSources {
			if acks[alias] != (deploysource.SourceEnrollmentResult{Schema: 1, SourceID: s.SourceID, WorkerID: response.WorkerID}) {
				return last, fmt.Errorf("source retirement result differs")
			}
		}
		if err = write(WorkerSetupStep{Operation: "sources-retired", RetiredSources: acks}); err != nil {
			return last, err
		}
		complete, err := read()
		if err != nil {
			return last, err
		}
		if complete.Stage != "complete" {
			return last, fmt.Errorf("source retirement did not commit")
		}
		return complete, nil
	}

	if err := write(request); err != nil {
		return last, err
	}
	response, err := read()
	if err != nil {
		return last, err
	}
	if request.Operation == "reserve" {
		if response.Stage != "reserved" || response.Producer == nil || response.Producer.Validate(response.WorkerID) != nil || len(response.PublicKeys) != 0 {
			return last, fmt.Errorf("invalid backup reservation response")
		}
		return response, nil
	}
	if request.Operation == "adopt" {
		if response.Stage != "complete" || !response.Unchanged || len(response.PublicKeys) != 0 || response.Producer != nil || response.PolicyUpdateRequired {
			return last, fmt.Errorf("adoption must preserve existing source enrollment without setup actions")
		}
		return response, nil
	}
	if response.Stage == "retiring-sources" {
		return finishRetirement(response)
	}
	if response.Stage == "complete" {
		return response, nil
	}
	if response.Stage != "prepared" || len(response.PublicKeys) != len(request.Sources) {
		return last, fmt.Errorf("invalid prepared setup response")
	}
	for _, s := range request.Sources {
		public, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(response.PublicKeys[s.Alias]))
		if err != nil || len(rest) != 0 || public.Type() != ssh.KeyAlgoED25519 {
			return last, fmt.Errorf("invalid prepared source key set")
		}
	}
	for alias, retained := range response.ReadOnlySources {
		public, _, _, _, err := ssh.ParseAuthorizedKey([]byte(response.PublicKeys[alias]))
		if err != nil || retained.Schema != 1 || retained.WorkerID != response.WorkerID || !safefs.ValidID(retained.SourceID, "s") || retained.Fingerprint != ssh.FingerprintSHA256(public) || inspect == nil {
			return last, fmt.Errorf("invalid retained source inspection authority")
		}
	}
	for _, s := range request.Sources {
		key := response.PublicKeys[s.Alias]
		public, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(key))
		if err != nil || len(rest) != 0 || public.Type() != ssh.KeyAlgoED25519 {
			return last, fmt.Errorf("invalid prepared source key")
		}
		var enrolled deploysource.SourceEnrollmentResult
		if retained, ok := response.ReadOnlySources[s.Alias]; ok {
			enrolled, err = inspect(s, response.WorkerID, key, retained)
			if err == nil && enrolled != retained {
				return last, fmt.Errorf("source endpoint does not retain the enrolled identity")
			}
		} else {
			enrolled, err = enroll(s, response.WorkerID, key)
		}
		if err != nil {
			return last, err
		}
		if enrolled.Schema != 1 || enrolled.WorkerID != response.WorkerID || !safefs.ValidID(enrolled.SourceID, "s") || enrolled.Fingerprint != ssh.FingerprintSHA256(public) {
			return last, fmt.Errorf("source enrollment identity mismatch")
		}
		if err = write(WorkerSetupStep{Operation: "source", Alias: s.Alias, Enrollment: &enrolled}); err != nil {
			return last, err
		}
		ack, err := read()
		if err != nil {
			return last, err
		}
		if ack.Stage != "source-recorded" {
			return last, fmt.Errorf("invalid source enrollment acknowledgement")
		}
	}
	if err = write(WorkerSetupStep{Operation: "finish"}); err != nil {
		return last, err
	}
	response, err = read()
	if err != nil {
		return last, err
	}
	if response.Stage == "retiring-sources" {
		return finishRetirement(response)
	}
	if response.Stage != "complete" {
		return last, fmt.Errorf("setup did not commit")
	}
	return response, nil
}

func validSetupOperation(r WorkerSetupRequest) bool {
	switch r.Operation {
	case "", "setup", "update":
		return true
	case "apply":
		return r.Declaration != nil
	case "reserve", "adopt":
		return r.Declaration != nil && !r.Recover && !r.Fenced
	default:
		return false
	}
}
