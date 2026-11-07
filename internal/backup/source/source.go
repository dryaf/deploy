package source

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/config"
	deployinventory "github.com/dryaf/deploy/internal/inventory"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/sqlite"
	"golang.org/x/sys/unix"
)

const sourceSnapshotCap = 5 * time.Minute

const sourceSessionCap = 17 * time.Minute

const sourceByteCap int64 = 10 << 30

var sourceReleaseIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,256}$`)

type SourceAuthorization struct {
	RetirementReason string `json:"retirement_reason,omitempty"`
	Previous         string `json:"previous,omitempty"`
	Enabled          bool   `json:"enabled"`
	Fingerprint      string `json:"fingerprint"`
}

type SourceIdentity struct {
	Schema      int                            `json:"schema"`
	ID          string                         `json:"id"`
	Workers     map[string]SourceAuthorization `json:"workers"`
	Generations map[string]string              `json:"generations"`
}

type SourceGeneration struct {
	Schema      int                     `json:"schema"`
	Generation  string                  `json:"generation"`
	BindingHash string                  `json:"binding_hash"`
	Metadata    quadlet.QuadletMetadata `json:"metadata"`
}

type SourceRequest struct {
	Protocol  int                       `json:"protocol"`
	Operation string                    `json:"operation"`
	Attempt   *protocol.SnapshotAttempt `json:"attempt,omitempty"`
}

type SourceInstance struct {
	Metadata    quadlet.QuadletMetadata `json:"metadata"`
	BindingHash string                  `json:"binding_hash"`
	Generation  string                  `json:"generation"`
	Eligible    bool                    `json:"eligible"`
	Diagnostic  string                  `json:"diagnostic,omitempty"`
}

type SourceInventory struct {
	Protocol    int              `json:"protocol"`
	SourceID    string           `json:"source_id"`
	Complete    bool             `json:"complete"`
	Instances   []SourceInstance `json:"instances"`
	Diagnostics []string         `json:"diagnostics"`
}

func TrustedSourceHome() (string, error) {
	account, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return "", err
	}
	if account.Uid == "0" || !filepath.IsAbs(account.HomeDir) {
		return "", fmt.Errorf("source helper requires a rootless deployment account")
	}
	return account.HomeDir, nil
}

func SourceProcessEnvironment(home string) []string {
	runtime := "/run/user/" + strconv.Itoa(os.Getuid())
	return []string{"HOME=" + home, "USER=" + filepath.Base(home), "PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_RUNTIME_DIR=" + runtime, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtime + "/bus"}
}

func loadSourceIdentity(home, worker string) (*safefs.Directory, SourceIdentity, error) {
	var identity SourceIdentity
	if !safefs.ValidID(worker, "w") {
		return nil, identity, fmt.Errorf("invalid enrolled worker identity")
	}
	dir, err := safefs.Open(filepath.Join(home, ".local/state/deploy/backup-source"))
	if err != nil {
		return nil, identity, err
	}
	if err = dir.ReadJSON("identity.json", &identity); err != nil {
		dir.Close()
		return nil, identity, err
	}
	if identity.Schema != 1 || !safefs.ValidID(identity.ID, "s") || !identity.Workers[worker].Enabled {
		dir.Close()
		return nil, identity, fmt.Errorf("worker is not authorized on this source")
	}
	return dir, identity, nil
}

func Serve(worker, keyID string) error {
	if os.Getenv("SSH_ORIGINAL_COMMAND") != "" {
		return fmt.Errorf("source key does not accept commands, shell, SCP or SFTP")
	}
	home, err := TrustedSourceHome()
	if err != nil {
		return err
	}
	dir, auth, err := loadSourceIdentity(home, worker)
	if err != nil {
		return err
	}
	dir.Close()
	if auth.Workers[worker].Fingerprint != keyID {
		return fmt.Errorf("source key generation revoked")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	// Disconnecting the SSH client does not cancel the source-owned watchdog.
	signal.Ignore(syscall.SIGHUP)
	cmd := exec.Command(executable, "__source_session", worker, keyID)
	cmd.Env = SourceProcessEnvironment(home)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return process.Run(cmd, sourceSessionCap, 10*time.Second)
}

func Session(worker, keyID string) error {
	home, err := TrustedSourceHome()
	if err != nil {
		return err
	}
	dir, identity, err := loadSourceIdentity(home, worker)
	if err != nil {
		return err
	}
	defer dir.Close()
	if identity.Workers[worker].Fingerprint != keyID {
		return fmt.Errorf("source key generation revoked")
	}
	sessionLock, err := dir.Lock("session.lock", true, false)
	if err != nil {
		return err
	}
	defer sessionLock.Close()
	// Closing protocol descriptors interrupts a stalled read/write while capture
	// has its own independent child deadline and file-size quota.
	timer := time.AfterFunc(sourceSessionCap, func() { os.Stdin.Close(); os.Stdout.Close() })
	defer timer.Stop()
	requestBytes, err := protocol.ReadBackupFrame(os.Stdin)
	if err != nil {
		return err
	}
	var request SourceRequest
	if err = protocol.StrictBackupJSON(requestBytes, &request); err != nil {
		return err
	}
	if request.Protocol != protocol.BackupProtocol {
		return fmt.Errorf("unsupported source protocol")
	}
	if err = AdmitSourceRequest(dir, identity, worker); err != nil {
		return err
	}
	switch request.Operation {
	case "capabilities":
		if request.Attempt != nil {
			return fmt.Errorf("unexpected request fields")
		}
		body, _ := json.Marshal(struct {
			Protocol int    `json:"protocol"`
			SourceID string `json:"source_id"`
			MaxBytes int64  `json:"max_snapshot_bytes"`
			Format   string `json:"format"`
		}{1, identity.ID, sourceByteCap, "sqlite-delete-v1"})
		return protocol.WriteBackupFrame(os.Stdout, body)
	case "inventory":
		if request.Attempt != nil {
			return fmt.Errorf("unexpected request fields")
		}
		inventory, err := CollectInventory(home, dir, identity)
		if err != nil {
			return err
		}
		body, _ := json.Marshal(inventory)
		return protocol.WriteBackupFrame(os.Stdout, body)
	case "snapshot":
		if request.Attempt == nil {
			return fmt.Errorf("missing snapshot attempt")
		}
		if err = request.Attempt.Validate(); err != nil {
			return err
		}
		if request.Attempt.SourceID != identity.ID {
			return fmt.Errorf("source identity changed")
		}
		return sourceExport(home, dir, identity, worker, *request.Attempt)
	default:
		return fmt.Errorf("source operation is not allowed")
	}
}

func CollectInventory(home string, dir *safefs.Directory, identity SourceIdentity) (SourceInventory, error) {
	inventory, err := deployinventory.Collect(nil)
	if err != nil {
		return SourceInventory{}, err
	}
	result := SourceInventory{Protocol: 1, SourceID: identity.ID, Complete: inventory.Complete, Instances: []SourceInstance{}, Diagnostics: inventory.Diagnostics}
	for _, entry := range inventory.Entries {
		m := entry.Metadata
		if m == nil || m.Role != "application" || m.Backup == nil || !m.Backup.Enabled {
			continue
		}
		item := SourceInstance{Metadata: *m, BindingHash: protocol.MetadataBindingHash(*m), Generation: "unenrolled"}
		if entry.Management != "deploy" || entry.Ownership != "confirmed" || entry.State == "shadowed" || entry.State == "masked" || entry.Dropins {
			item.Diagnostic = "managed ownership/runtime source is not confirmed"
		} else if entry.SourcePath != filepath.Join(home, ".config/containers/systemd", m.Service+".container") {
			item.Diagnostic = "unsupported Quadlet source path"
		} else {
			var generation SourceGeneration
			err := dir.ReadJSON("generations/"+safefs.LockName(m.Service)+".json", &generation)
			if err == nil {
				item.Generation = generation.Generation
				if generation.Schema != 1 || !safefs.ValidID(generation.Generation, "g") || generation.BindingHash != item.BindingHash || (identity.Generations[m.Service] != "" && identity.Generations[m.Service] != generation.Generation) {
					item.Diagnostic = "database generation/binding requires reconciliation"
				} else {
					// A durable generation with no ledger entry is an interrupted first
					// enrollment. Export repairs the ledger under policy/deployment locks;
					// inventory never writes or creates a replacement generation.
					item.Eligible = true
				}
			} else if errors.Is(err, os.ErrNotExist) && identity.Generations[m.Service] == "" {
				item.Eligible = true
			} else {
				item.Diagnostic = "generation evidence missing or corrupt"
			}
		}
		result.Instances = append(result.Instances, item)
	}
	return result, nil
}

func acquireSourceDeploymentLocks(home string, m quadlet.QuadletMetadata) ([]*safefs.Lock, error) {
	dir, err := safefs.Open(filepath.Join(home, ".local/state/deploy/locks"))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	var held []*safefs.Lock
	for _, name := range []string{safefs.LockName(m.Service) + ".lock", "target-" + safefs.LockName(m.TargetDir) + ".lock"} {
		lock, err := dir.Lock(name, true, false)
		if err != nil {
			for _, l := range held {
				l.Close()
			}
			return nil, err
		}
		held = append(held, lock)
	}
	return held, nil
}

func readSourceJSON(path string, dest any) error {
	dir, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	file, err := dir.Open(filepath.Base(path), unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, protocol.MaxBackupHeader+1))
	if err != nil {
		return err
	}
	if len(data) > protocol.MaxBackupHeader {
		return fmt.Errorf("source record exceeds limit")
	}
	if err = safefs.RejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	return json.Unmarshal(data, dest)
}

func sourceOwnership(home string, m quadlet.QuadletMetadata) (string, []byte, error) {
	if err := quadlet.ValidateMetadata(m); err != nil {
		return "", nil, err
	}
	locks := filepath.Join(home, ".local/state/deploy/locks")
	for _, name := range []string{safefs.LockName(m.Service) + ".owner.json", "target-" + safefs.LockName(m.TargetDir) + ".owner.json"} {
		var owner deployinventory.InventoryOwner
		if err := readSourceJSON(filepath.Join(locks, name), &owner); err != nil {
			return "", nil, err
		}
		if owner.Schema != 1 || owner.Service != m.Service || owner.Target != m.TargetDir || owner.Mode != m.Mode {
			return "", nil, fmt.Errorf("positive service/target ownership does not match metadata")
		}
	}
	root, err := safefs.Open(m.TargetDir)
	if err != nil {
		return "", nil, err
	}
	defer root.Close()
	for _, mode := range []string{"binary", "image", "db"} {
		for _, name := range []string{"pending.json", "boot-pending.json"} {
			file, err := root.Open(filepath.Join(".deploy", mode, name), unix.O_RDONLY|unix.O_NONBLOCK, 0)
			if err == nil {
				file.Close()
				return "", nil, fmt.Errorf("pending deployment/database recovery blocks source export")
			}
			if !errors.Is(err, os.ErrNotExist) {
				return "", nil, err
			}
		}
	}
	statePath := filepath.Join(m.TargetDir, ".deploy", string(m.Mode))
	var current deployinventory.InventoryOwner
	if err = readSourceJSON(filepath.Join(statePath, "current.json"), &current); err != nil {
		return "", nil, err
	}
	if current.Schema != 1 || current.Service != m.Service || current.Target != m.TargetDir || !sourceReleaseIDPattern.MatchString(current.Current) {
		return "", nil, fmt.Errorf("invalid confirmed release identity")
	}
	var manifest struct {
		Schema      int    `json:"schema"`
		Service     string `json:"service"`
		Target      string `json:"target"`
		CheckResult string `json:"check_result"`
		Database    string `json:"database"`
		UID         int    `json:"container_uid"`
		GID         int    `json:"container_gid"`
	}
	releaseDir := filepath.Join(statePath, "releases", current.Current)
	if err = readSourceJSON(filepath.Join(releaseDir, "manifest.json"), &manifest); err != nil {
		return "", nil, err
	}
	if manifest.Schema != 1 || manifest.Service != m.Service || manifest.Target != m.TargetDir || manifest.CheckResult == "" || manifest.Database != filepath.Join(m.TargetDir, m.Backup.Source) || manifest.UID != m.Backup.ContainerUID || manifest.GID != m.Backup.ContainerGID {
		return "", nil, fmt.Errorf("confirmed release database binding differs from metadata")
	}
	unitDir, err := safefs.Open(filepath.Join(home, ".config/containers/systemd"))
	if err != nil {
		return "", nil, err
	}
	defer unitDir.Close()
	unit, err := unitDir.Open(m.Service+".container", unix.O_RDONLY, 0)
	if err != nil {
		return "", nil, err
	}
	body, err := io.ReadAll(io.LimitReader(unit, quadlet.MaxQuadletBytes+1))
	unit.Close()
	if err != nil || len(body) > quadlet.MaxQuadletBytes {
		return "", nil, fmt.Errorf("installed unit cannot be read safely")
	}
	actual, err := quadlet.ParseMetadata(bytes.NewReader(body))
	if err != nil || actual == nil || protocol.MetadataBindingHash(*actual) != protocol.MetadataBindingHash(m) {
		return "", nil, fmt.Errorf("installed database binding changed")
	}
	rd, err := safefs.Open(releaseDir)
	if err != nil {
		return "", nil, err
	}
	defer rd.Close()
	candidate, err := rd.Open("candidate.container", unix.O_RDONLY, 0)
	if err != nil {
		return "", nil, err
	}
	committed, err := io.ReadAll(io.LimitReader(candidate, quadlet.MaxQuadletBytes+1))
	candidate.Close()
	if err != nil || !bytes.Equal(body, committed) {
		return "", nil, fmt.Errorf("installed Quadlet differs from committed candidate")
	}
	return current.Current, body, nil
}

func sourceEffectiveProfile(home string, m quadlet.QuadletMetadata, unit []byte, release string) error {
	inventory, err := deployinventory.Collect(nil)
	if err != nil {
		return err
	}
	verified := false
	containerID := ""
	for _, entry := range inventory.Entries {
		if entry.SourcePath == filepath.Join(home, ".config/containers/systemd", m.Service+".container") && entry.Management == "deploy" && entry.Ownership == "confirmed" && !entry.Dropins && entry.State != "shadowed" && entry.State != "masked" {
			verified = true
			containerID = entry.ContainerID
		}
	}
	if !verified {
		return fmt.Errorf("effective source, ownership or Quadlet drop-ins changed")
	}

	// Interpret only our narrow declared profile. Systemd proves the effective
	// generated file; unknown extensions and any drop-ins are ineligible.
	env := config.Environment{User: "source", Dir: m.TargetDir, Database: config.DatabaseConfig{Driver: m.Backup.Driver, Source: m.Backup.Source, Backup: config.DatabaseBackupConfig{Enabled: true}}}
	for _, line := range strings.Split(string(unit), "\n") {
		if strings.HasPrefix(line, "Volume=") {
			volume := strings.TrimPrefix(line, "Volume=")
			mountSource, _, _ := strings.Cut(volume, ":")
			for _, protected := range []string{filepath.Join(home, ".ssh"), filepath.Join(home, ".local/state/deploy"), filepath.Join(home, ".local/lib/deploy"), filepath.Join(home, ".config/containers/systemd"), filepath.Join(home, ".config/systemd"), "/proc", "/sys", filepath.Join("/run/user", strconv.Itoa(os.Getuid()))} {
				if config.PathsOverlap(mountSource, protected) {
					return fmt.Errorf("application bind exposes source-helper/control paths")
				}
			}
			env.Quadlet.Volumes = append(env.Quadlet.Volumes, strings.TrimPrefix(line, "Volume="))
		}
		for _, prefix := range []string{"PodmanArgs=", "UserNS=", "UIDMap=", "GIDMap=", "User=", "Group=", "Mount=", "AddDevice="} {
			if strings.HasPrefix(line, prefix) {
				return fmt.Errorf("source has an unsupported runtime override")
			}
		}
	}
	if err := config.ValidateBackupBinding(env); err != nil {
		return err
	}
	cmd := exec.Command("/usr/bin/systemctl", "--user", "show", "--no-pager", "--property=SourcePath,FragmentPath,DropInPaths,LoadState", m.Service+".service")
	cmd.Env = SourceProcessEnvironment(home)
	output, err := process.LimitedOutput(cmd, 5*time.Second, 64<<10)
	if err != nil {
		return fmt.Errorf("effective unit unavailable")
	}
	properties := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			properties[k] = v
		}
	}
	expected := filepath.Join(home, ".config/containers/systemd", m.Service+".container")
	if properties["SourcePath"] != expected || properties["DropInPaths"] != "" || properties["LoadState"] != "loaded" {
		return fmt.Errorf("effective source/drop-ins are outside the supported profile")
	}
	runtime := filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "systemd")
	fragment := properties["FragmentPath"]
	if !strings.HasPrefix(fragment, runtime+"/generator/") {
		return fmt.Errorf("effective unit is not the rootless generated Quadlet")
	}
	generator := "/usr/lib/systemd/system-generators/podman-system-generator"
	if _, err = os.Stat(generator); err != nil {
		generator = "/usr/libexec/podman/quadlet"
	}
	generate := exec.Command(generator, "--user", "--dryrun")
	generate.Env = SourceProcessEnvironment(home)
	var rendered, errs process.Buffer
	rendered.Limit = 4 << 20
	errs.Limit = 4096
	generate.Stdout = &rendered
	generate.Stderr = &errs
	if err = process.Run(generate, 20*time.Second, 2*time.Second); err != nil {
		return fmt.Errorf("cannot establish effective generated configuration")
	}
	marker := "---" + m.Service + ".service---\n"
	_, body, ok := strings.Cut(rendered.String(), marker)
	if !ok {
		return fmt.Errorf("generator did not return the selected service")
	}
	if next := strings.Index(body, "\n---"); next >= 0 {
		body = body[:next]
	}
	directory, err := safefs.Open(filepath.Dir(fragment))
	if err != nil {
		return err
	}
	defer directory.Close()
	file, err := directory.Open(filepath.Base(fragment), unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	actual, err := io.ReadAll(io.LimitReader(file, quadlet.MaxQuadletBytes+1))
	file.Close()
	if err != nil || strings.TrimSpace(string(actual)) != strings.TrimSpace(body) {
		return fmt.Errorf("effective generated unit differs from the installed Quadlet")
	}
	return verifySourceRuntimeBinding(home, m, env, release, containerID)
}

type snapshotJobResult struct {
	Header  protocol.SnapshotHeader `json:"header"`
	SpoolID string                  `json:"spool_id"`
}

func sourceExport(home string, dir *safefs.Directory, identity SourceIdentity, worker string, attempt protocol.SnapshotAttempt) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, "__source_job", worker, identity.Workers[worker].Fingerprint)
	cmd.Env = SourceProcessEnvironment(home)
	raw, _ := json.Marshal(attempt)
	cmd.Stdin = bytes.NewReader(raw)
	output, err := process.LimitedOutput(cmd, sourceSnapshotCap+time.Minute, protocol.MaxBackupHeader)
	if err != nil {
		return fmt.Errorf("independent source capture job failed: %w", err)
	}
	var result snapshotJobResult
	if err = protocol.StrictBackupJSON(output, &result); err != nil {
		return err
	}
	if !safefs.ValidID(result.SpoolID, "b") || result.Header.SnapshotID != result.SpoolID {
		return fmt.Errorf("invalid source job identity")
	}
	spool := filepath.Join(dir.File().Name(), "exports", result.SpoolID)
	defer CleanupSourceSpool(spool)
	if err = protocol.ValidateSnapshotHeader(result.Header, attempt, sourceByteCap); err != nil {
		return err
	}
	currentDir, current, err := loadSourceIdentity(home, worker)
	if err != nil {
		return err
	}
	currentDir.Close()
	if current.ID != identity.ID || current.Workers[worker].Fingerprint != identity.Workers[worker].Fingerprint {
		return fmt.Errorf("source authorization changed before streaming")
	}
	snapshotDir, err := dir.Sub("exports/" + result.SpoolID)
	if err != nil {
		return err
	}
	defer snapshotDir.Close()
	file, err := snapshotDir.Open("database.db", unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if err = AdmitSourceTransfer(dir, identity, worker, result.Header.Size); err != nil {
		return err
	}
	return protocol.WriteSnapshotTransfer(os.Stdout, result.Header, file)
}

func CaptureJob(worker, keyID string) error {
	home, err := TrustedSourceHome()
	if err != nil {
		return err
	}
	dir, identity, err := loadSourceIdentity(home, worker)
	if err != nil {
		return err
	}
	defer dir.Close()
	if identity.Workers[worker].Fingerprint != keyID {
		return fmt.Errorf("source key generation changed")
	}
	var attempt protocol.SnapshotAttempt
	if err = safefs.ReadJSON(os.Stdin, &attempt, protocol.MaxBackupHeader); err != nil {
		return err
	}
	if err = attempt.Validate(); err != nil {
		return err
	}
	if attempt.SourceID != identity.ID {
		return fmt.Errorf("source identity changed")
	}
	result, err := sourceCaptureJob(home, dir, identity, worker, attempt)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func sourceCaptureJob(home string, dir *safefs.Directory, identity SourceIdentity, worker string, attempt protocol.SnapshotAttempt) (result snapshotJobResult, err error) {
	exportLock, err := dir.Lock("export.lock", true, false)
	if err != nil {
		return result, err
	}
	defer exportLock.Close()
	if err = cleanupExpiredSourceSpools(dir, identity.ID); err != nil {
		return result, err
	}
	inventory, err := CollectInventory(home, dir, identity)
	if err != nil {
		return result, err
	}
	var metadata *quadlet.QuadletMetadata
	for _, instance := range inventory.Instances {
		if instance.Metadata.Service == attempt.Service && instance.Eligible && instance.BindingHash == attempt.BindingHash {
			copy := instance.Metadata
			metadata = &copy
			break
		}
	}
	if metadata == nil {
		return result, fmt.Errorf("requested service is not eligible for export")
	}
	held, err := acquireSourceDeploymentLocks(home, *metadata)
	if err != nil {
		return result, err
	}
	defer func() {
		for _, lock := range held {
			lock.Close()
		}
	}()
	release, unit, err := sourceOwnership(home, *metadata)
	if err != nil {
		return result, err
	}
	if err = sourceEffectiveProfile(home, *metadata, unit, release); err != nil {
		return result, err
	}
	generation, err := SourceGenerationForExport(dir, identity, worker, *metadata, attempt)
	if err != nil {
		return result, err
	}
	if err = dir.Capacity(sourceByteCap + 1<<30); err != nil {
		return result, err
	}
	id, err := safefs.NewID("b")
	if err != nil {
		return result, err
	}
	spool := filepath.Join(dir.File().Name(), "exports", id)
	defer func() {
		if result.SpoolID == "" {
			// A killed/timed-out supervisor can leave bounded capture children.
			// Their inherited export lease fences cleanup until they have exited.
			exportLock.Close()
			cleanupLease, e := dir.Lock("export.lock", true, false)
			if e == nil {
				defer cleanupLease.Close()
				CleanupSourceSpool(spool)
			}
		}
	}()
	if err = WriteSourceSpoolOwner(spool, identity.ID, id, attempt.Nonce); err != nil {
		return result, err
	}
	executable, err := os.Executable()
	if err != nil {
		return result, err
	}
	source := filepath.Join(metadata.TargetDir, metadata.Backup.Source)
	args := []string{executable, "__source_capture", source, filepath.Join(spool, "database.db"), strconv.FormatInt(sourceByteCap, 10)}
	if metadata.Backup.ContainerUID > 0 {
		args = append([]string{"/usr/bin/podman", "unshare"}, args...)
	}
	// The native watchdog survives loss of this Go supervisor. Both it and
	// every database subprocess retain the export/service/target lock FDs.
	args = append([]string{"--signal=TERM", "--kill-after=10s", fmt.Sprintf("%.3fs", (sourceSnapshotCap + 30*time.Second).Seconds())}, args...)
	capture := exec.Command("/usr/bin/timeout", args...)
	capture.ExtraFiles = []*os.File{exportLock.File(), held[0].File(), held[1].File()}
	capture.Env = SourceProcessEnvironment(home)
	var stdout, stderr process.Buffer
	stdout.Limit = 4096
	stderr.Limit = 4096
	capture.Stdout = &stdout
	capture.Stderr = &stderr
	charge, err := ReserveSourceCapture(dir, identity, worker)
	if err != nil {
		return result, err
	}
	defer finishSourceCapture(dir, identity, worker, charge)
	started := time.Now().UTC()
	if err = process.Run(capture, sourceSnapshotCap+30*time.Second, 30*time.Second); err != nil {
		return result, fmt.Errorf("bounded source capture failed: %w", err)
	}
	finished := time.Now().UTC()
	for _, lock := range held {
		lock.Close()
	}
	held = nil
	file, err := os.Open(filepath.Join(spool, "database.db"))
	if err != nil {
		return result, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() > sourceByteCap {
		return result, fmt.Errorf("source snapshot exceeds size policy")
	}
	digest, err := safefs.DigestFile(file)
	if err != nil {
		return result, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	currentDir, current, err := loadSourceIdentity(home, worker)
	if err != nil {
		return result, err
	}
	currentDir.Close()
	if current.ID != identity.ID || current.Workers[worker].Fingerprint != identity.Workers[worker].Fingerprint {
		return result, fmt.Errorf("source identity changed before streaming")
	}
	header := protocol.SnapshotHeader{Protocol: 1, Role: "source-snapshot", Attempt: attempt, SnapshotID: id, Generation: generation, Metadata: *metadata, ReleaseID: release, StartedAt: started, FinishedAt: finished, Size: info.Size(), SHA256: digest, Format: "sqlite-delete-v1", SQLiteVersion: strings.TrimSpace(stdout.String())}
	result = snapshotJobResult{Header: header, SpoolID: id}
	return result, nil
}

func SourceGenerationForExport(dir *safefs.Directory, identity SourceIdentity, worker string, m quadlet.QuadletMetadata, attempt protocol.SnapshotAttempt) (string, error) {
	lock, err := dir.Lock("policy.lock", true, false)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err = dir.ReadJSON("identity.json", &identity); err != nil {
		return "", err
	}
	if !identity.Workers[worker].Enabled {
		return "", fmt.Errorf("worker authorization revoked")
	}
	name := "generations/" + safefs.LockName(m.Service) + ".json"
	var generation SourceGeneration
	err = dir.ReadJSON(name, &generation)
	if errors.Is(err, os.ErrNotExist) {
		if attempt.Generation != "unenrolled" || identity.Generations[m.Service] != "" {
			return "", fmt.Errorf("known source generation disappeared; explicit reconciliation required")
		}
		id, e := safefs.NewID("g")
		if e != nil {
			return "", e
		}
		generation = SourceGeneration{1, id, protocol.MetadataBindingHash(m), m}
		if e = dir.WriteJSON(name, generation, false); e != nil {
			return "", e
		}
	} else if err != nil {
		return "", err
	}
	if generation.Schema != 1 || generation.BindingHash != attempt.BindingHash || !safefs.ValidID(generation.Generation, "g") || (attempt.Generation != "unenrolled" && attempt.Generation != generation.Generation) {
		return "", fmt.Errorf("database generation/binding changed; explicit reconciliation required")
	}
	if known := identity.Generations[m.Service]; known != "" && known != generation.Generation {
		return "", fmt.Errorf("source generation ledger conflict")
	}
	if identity.Generations == nil {
		identity.Generations = map[string]string{}
	}
	identity.Generations[m.Service] = generation.Generation
	if err = dir.WriteJSON("identity.json", identity, true); err != nil {
		return "", err
	}
	return generation.Generation, nil
}

func Capture(source, destination string, maxBytes int64) error {
	locks, err := sqlite.InheritSourceCaptureLocks()
	if err != nil {
		return err
	}
	defer func() {
		for _, lock := range locks {
			lock.Close()
		}
	}()
	sqlite.ConfigureSourceCapture(locks)
	if maxBytes < 4096 || maxBytes > sourceByteCap {
		return fmt.Errorf("invalid source size budget")
	}
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &unix.Rlimit{Cur: uint64(maxBytes), Max: uint64(maxBytes)}); err != nil {
		return err
	}
	// Check path components inside the same rootless namespace used by SQLite.
	dir, err := safefs.Open(filepath.Dir(source))
	if err != nil {
		return err
	}
	file, err := dir.Open(filepath.Base(source), unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		dir.Close()
		return err
	}
	file.Close()
	dir.Close()
	if err = sqlite.Snapshot(source, destination, sourceSnapshotCap); err != nil {
		return err
	}
	output, err := sqlite.Output([]string{"-readonly", destination, "SELECT sqlite_version();"}, time.Second)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(output)
	return err
}
