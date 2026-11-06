package backup

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/helper"
	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/backup/protocol"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

type SetupSource struct {
	Alias      string `json:"alias"`
	Host       string `json:"host"`
	User       string `json:"user"`
	Port       int    `json:"port"`
	KnownHosts string `json:"known_hosts"`
}

type WorkerSetupRequest struct {
	Recover    bool                      `json:"recover,omitempty"`
	Fenced     bool                      `json:"fenced,omitempty"`
	Operation  string                    `json:"operation,omitempty"`
	Schema     int                       `json:"schema"`
	Policy     config.BackupWorkerConfig `json:"policy"`
	Sources    []SetupSource             `json:"sources"`
	Image      string                    `json:"image"`
	RelayImage string                    `json:"relay_image"`
}

type WorkerSetupJournal struct {
	Shared              *sharedWorkerSetup                             `json:"shared,omitempty"`
	RecoveryNeeded      bool                                           `json:"recovery_needed,omitempty"`
	ArchivePath         string                                         `json:"archive_path,omitempty"`
	BeforeInstallation  string                                         `json:"before_installation,omitempty"`
	BeforeHelper        *helper.HelperArtifactManifest                 `json:"before_helper,omitempty"`
	CandidateHelper     string                                         `json:"candidate_helper,omitempty"`
	Updating            bool                                           `json:"updating"`
	BeforeControl       *ArchiveControl                                `json:"before_control,omitempty"`
	BeforeMinFreeBytes  *int64                                         `json:"before_min_free_bytes,omitempty"`
	PreviousRequestHash string                                         `json:"previous_request_hash,omitempty"`
	EnabledUnits        []string                                       `json:"enabled_units,omitempty"`
	StateInitializing   bool                                           `json:"state_initializing"`
	StateInitialized    bool                                           `json:"state_initialized"`
	Schema              int                                            `json:"schema"`
	ID                  string                                         `json:"id"`
	RequestHash         string                                         `json:"request_hash"`
	WorkerID            string                                         `json:"worker_id"`
	ArchiveID           string                                         `json:"archive_id"`
	Stage               string                                         `json:"stage"`
	SourceEnrollments   map[string]deploysource.SourceEnrollmentResult `json:"source_enrollments"`
	Files               map[string]setupFileChange                     `json:"files"`
	OwnedFiles          map[string]string                              `json:"owned_files"`
}

type setupFileChange struct {
	Before    []byte `json:"before"`
	Existed   bool   `json:"existed"`
	AfterHash string `json:"after_hash"`
}

type WorkerSetupResponse struct {
	Schema     int               `json:"schema"`
	Stage      string            `json:"stage"`
	WorkerID   string            `json:"worker_id"`
	ArchiveID  string            `json:"archive_id"`
	PublicKeys map[string]string `json:"public_keys"`
	Error      string            `json:"error,omitempty"`
}

type WorkerSetupStep struct {
	Operation  string                               `json:"operation"`
	Alias      string                               `json:"alias,omitempty"`
	Enrollment *deploysource.SourceEnrollmentResult `json:"enrollment,omitempty"`
}

func SendSetupFrame(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return protocol.WriteBackupFrame(os.Stdout, body)
}

func WorkerSetupCoordinator() error {
	timer := time.AfterFunc(30*time.Minute, func() { os.Stdin.Close(); os.Stdout.Close() })
	defer timer.Stop()
	raw, err := protocol.ReadBackupFrame(os.Stdin)
	if err != nil {
		return err
	}
	var request WorkerSetupRequest
	if err = protocol.StrictBackupJSON(raw, &request); err != nil {
		return err
	}
	if request.Recover && !request.Fenced {
		return fmt.Errorf("recovery requires --fenced acknowledgement that the former writer is disabled")
	}
	if request.Schema != 1 {
		return fmt.Errorf("unsupported setup protocol")
	}
	request.Policy, err = config.NormalizeWorker(request.Policy)
	if err != nil {
		return err
	}
	if err = config.PinnedWorkerImage(request.Image); err != nil {
		return err
	}
	if request.Policy.Notifications.Signal != nil {
		if err = config.PinnedWorkerImage(request.RelayImage); err != nil {
			return err
		}
	}
	home, err := deploysource.TrustedSourceHome()
	if err != nil {
		return err
	}
	if err = workerHostPreflight(home, request); err != nil {
		return err
	}
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.Mkdir(".local/state/deploy/locks"); err != nil {
		return err
	}
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, true)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = root.Mkdir(".local/state/deploy/backup-worker"); err != nil {
		return err
	}
	stateRoot, err := root.Sub(".local/state/deploy/backup-worker")
	if err != nil {
		return err
	}
	defer stateRoot.Close()
	if request.Operation != "setup" && request.Operation != "update" && request.Operation != "" {
		return fmt.Errorf("unsupported installation operation")
	}
	canonicalRequest := request
	canonicalRequest.Operation = ""
	canonical, _ := json.Marshal(canonicalRequest)
	requestHash := fmt.Sprintf("%x", sha256.Sum256(canonical))
	var journal WorkerSetupJournal
	err = stateRoot.ReadJSON("setup.json", &journal)
	if errors.Is(err, os.ErrNotExist) {
		journal.Schema = 1
		journal.ID, err = safefs.NewID("n")
		if err != nil {
			return err
		}
		journal.WorkerID, err = safefs.NewID("w")
		if err != nil {
			return err
		}
		journal.ArchiveID, err = safefs.NewID("a")
		if err != nil {
			return err
		}
		if request.Recover {
			recoveredState, e := openBackupState(filepath.Join(home, ".local/state/deploy/backup-worker/state"), false)
			if e != nil {
				return e
			}
			var mode string
			var control ArchiveControl
			e = recoveredState.get("mode", &mode)
			if e == nil {
				e = recoveredState.get("recovered_control", &control)
			}
			recoveredState.Close()
			if e != nil || mode != "recovery" {
				return fmt.Errorf("setup --recover needs a previously reconstructed read-mode catalog")
			}
			journal.ArchiveID = control.ArchiveID
			journal.StateInitialized = true
		}
		journal.RequestHash = requestHash
		journal.Stage = "started"
		journal.SourceEnrollments = map[string]deploysource.SourceEnrollmentResult{}
		journal.Files = map[string]setupFileChange{}
		journal.OwnedFiles = map[string]string{}
		if err = stateRoot.WriteJSON("setup.json", journal, false); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if (journal.Schema != 1 || journal.Shared != nil) && (journal.Schema != 2 || journal.Shared == nil) {
		return fmt.Errorf("unsupported setup journal")
	}
	if err = prepareSharedWorkerSetup(home, root, stateRoot, &journal, request.Operation); err != nil {
		return err
	}
	if err = requireWorkerSetupOwnership(home, &journal); err != nil {
		return err
	}
	if journal.RecoveryNeeded {
		if err = recoverWorkerSetupLocked(home, root, stateRoot, &journal); err != nil {
			return err
		}
	}
	if request.Operation == "update" && journal.Stage == "complete" && journal.RequestHash != requestHash {
		for path, hash := range journal.OwnedFiles {
			body, e := safefs.ReadFile(root, path, 8<<20)
			if e != nil || fmt.Sprintf("%x", sha256.Sum256(body)) != hash {
				return fmt.Errorf("installed worker file drift: %s", path)
			}
		}
		journal.PreviousRequestHash = journal.RequestHash
		journal.RequestHash = requestHash
		journal.Updating = true
		journal.Stage = "prepared"
		if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
			return err
		}
	}
	if journal.RequestHash != requestHash {
		return fmt.Errorf("installation policy differs from its setup journal; use explicit update/reconciliation before changing it")
	}
	if journal.Stage == "complete" {
		for path, hash := range journal.OwnedFiles {
			body, err := safefs.ReadFile(root, path, 8<<20)
			if err != nil || fmt.Sprintf("%x", sha256.Sum256(body)) != hash {
				return fmt.Errorf("installed worker file drift requires reconciliation: %s", path)
			}
		}
		return SendSetupFrame(WorkerSetupResponse{Schema: 1, Stage: "complete", WorkerID: journal.WorkerID, ArchiveID: journal.ArchiveID})
	}
	if journal.Stage == "installing" {
		if err = rollbackWorkerFiles(root, &journal); err != nil {
			return err
		}
		journal.Stage = "prepared"
		if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
			return err
		}
	}
	if err = root.Mkdir(".config/deploy/backup-worker/ssh"); err != nil {
		return err
	}
	keys, err := root.Sub(".config/deploy/backup-worker/ssh")
	if err != nil {
		return err
	}
	defer keys.Close()
	publicKeys := map[string]string{}
	for _, source := range request.Sources {
		if !config.ValidInventoryName(source.Alias) || source.User == "root" || !config.ValidInventoryName(source.User) || source.Host == "" || strings.ContainsAny(source.Host, " \r\n\t") || strings.HasPrefix(source.Host, "-") || source.Port < 1 || source.Port > 65535 || len(source.KnownHosts) > 16384 {
			return fmt.Errorf("invalid setup source")
		}
		keyFile := filepath.Join(home, ".config/deploy/backup-worker/ssh", source.Alias+".key")
		f, err := keys.Open(source.Alias+".key", unix.O_RDONLY, 0)
		if errors.Is(err, os.ErrNotExist) {
			cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "deploy-backup:"+journal.WorkerID+":"+source.Alias, "-f", keyFile)
			if _, err = process.Output(cmd, 10*time.Second); err != nil {
				return err
			}
			f, err = keys.Open(source.Alias+".key", unix.O_RDONLY, 0)
		}
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			f.Close()
			return fmt.Errorf("unsafe worker source key")
		}
		if err = f.Sync(); err != nil {
			f.Close()
			return err
		}
		if err = keys.File().Sync(); err != nil {
			f.Close()
			return err
		}
		key, err := io.ReadAll(io.LimitReader(f, 16385))
		f.Close()
		if err != nil || len(key) > 16384 {
			return fmt.Errorf("invalid worker source key")
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil || signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
			return fmt.Errorf("worker source identity is not Ed25519")
		}
		publicKeys[source.Alias] = string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
		if err = stageWorkerTrust(home, root, stateRoot, &journal, source.Alias, []byte(source.KnownHosts)); err != nil {
			return err
		}
	}
	mount, err := safefs.Open(request.Policy.Storage.RequireMount)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(request.Policy.Storage.RequireMount, request.Policy.Storage.Path)
	if err == nil && relative != "." {
		err = mount.Mkdir(relative)
	}
	mount.Close()
	if err != nil {
		return err
	}
	var archive *backupArchive
	if request.Recover {
		archive, err = openBackupArchive(request.Policy.Storage.Path)
	} else {
		archive, err = resumeArchiveEnrollment(request.Policy.Storage.Path, journal.ArchiveID, journal.WorkerID)
	}
	if err != nil {
		return err
	}
	defer archive.Close()
	journal.Stage = "prepared"
	if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
		return err
	}
	if err = SendSetupFrame(WorkerSetupResponse{Schema: 1, Stage: "prepared", WorkerID: journal.WorkerID, ArchiveID: journal.ArchiveID, PublicKeys: publicKeys}); err != nil {
		return err
	}
	for {
		raw, err = protocol.ReadBackupFrame(os.Stdin)
		if err != nil {
			return err
		}
		var step WorkerSetupStep
		if err = protocol.StrictBackupJSON(raw, &step); err != nil {
			return err
		}
		switch step.Operation {
		case "source":
			if publicKeys[step.Alias] == "" || step.Enrollment == nil || step.Enrollment.Schema != 1 || step.Enrollment.WorkerID != journal.WorkerID || !safefs.ValidID(step.Enrollment.SourceID, "s") {
				return fmt.Errorf("source enrollment response mismatch")
			}
			public, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKeys[step.Alias]))
			if err != nil || ssh.FingerprintSHA256(public) != step.Enrollment.Fingerprint {
				return fmt.Errorf("source enrolled a different key")
			}
			journal.SourceEnrollments[step.Alias] = *step.Enrollment
			if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
				return err
			}
			if err = SendSetupFrame(WorkerSetupResponse{Schema: 1, Stage: "source-recorded"}); err != nil {
				return err
			}
		case "finish":
			if err = finishWorkerSetup(home, root, stateRoot, &journal, request, archive); err != nil {
				return err
			}
			return SendSetupFrame(WorkerSetupResponse{Schema: 1, Stage: "complete", WorkerID: journal.WorkerID, ArchiveID: journal.ArchiveID})
		default:
			return fmt.Errorf("unsupported setup transition")
		}
	}
}

func workerHostPreflight(home string, request WorkerSetupRequest) error {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return fmt.Errorf("worker requires 64-bit AMD64 or ARM64 Linux")
	}
	for _, name := range []string{"podman", "systemctl", "loginctl", "ssh", "ssh-keygen", "bwrap"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("worker prerequisite missing: %s", name)
		}
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return fmt.Errorf("worker requires cgroup v2")
	}
	output, err := process.Output(exec.Command("loginctl", "show-user", fmt.Sprint(os.Getuid()), "--property=Linger", "--value"), 5*time.Second)
	if err != nil || strings.TrimSpace(string(output)) != "yes" {
		return fmt.Errorf("worker requires user lingering; administrator must enable it explicitly")
	}
	if _, err = process.Output(exec.Command("systemctl", "--user", "show-environment"), 5*time.Second); err != nil {
		return fmt.Errorf("rootless user systemd manager is unavailable")
	}
	mountData, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	mounted := false
	for _, line := range strings.Split(string(mountData), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 6 && fields[4] == request.Policy.Storage.RequireMount {
			mounted = true
			break
		}
	}
	if !mounted {
		return fmt.Errorf("required storage path is not an actual mountpoint")
	}
	dir, err := safefs.Open(request.Policy.Storage.RequireMount)
	if err != nil {
		return err
	}
	defer dir.Close()
	var fs unix.Statfs_t
	if err = unix.Fstatfs(int(dir.File().Fd()), &fs); err != nil {
		return err
	}
	if fs.Type != 0xef53 {
		return fmt.Errorf("v1 archive enrollment requires a local ext4 filesystem")
	}
	if err = dir.Capacity(request.Policy.MaxSnapshotBytes + request.Policy.Storage.MinFreeBytes); err != nil {
		return err
	}
	return nil
}

func resumeArchiveEnrollment(directory, archiveID, workerID string) (*backupArchive, error) {
	dir, err := safefs.Open(directory)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	var identity ArchiveIdentity
	err = dir.ReadJSON("archive.json", &identity)
	if errors.Is(err, os.ErrNotExist) {
		return enrollBackupArchive(directory, archiveID, workerID)
	}
	if err != nil {
		return nil, err
	}
	if identity.ID != archiveID || identity.WriterID != workerID || identity.Schema != 1 {
		return nil, fmt.Errorf("archive does not belong to this setup journal")
	}
	if identity.Enrolled {
		return openBackupArchive(directory)
	}
	current, err := dir.FilesystemID()
	if err != nil || current != identity.FilesystemID {
		return nil, fmt.Errorf("incomplete enrollment filesystem changed")
	}
	for _, name := range []string{".locks", ".incoming", ".trash", "sources", "control/verification"} {
		if err = dir.Mkdir(name); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"maintenance", "index", "control", "writer"} {
		lease, err := dir.Lock(".locks/"+name+".lock", true, true)
		if err != nil {
			return nil, err
		}
		lease.Close()
	}
	identity.Enrolled = true
	if err = dir.WriteJSON("archive.json", identity, true); err != nil {
		return nil, err
	}
	return openBackupArchive(directory)
}

func rollbackWorkerFiles(home *safefs.Directory, journal *WorkerSetupJournal) error {
	names := make([]string, 0, len(journal.Files))
	for name := range journal.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		change := journal.Files[name]
		parent, err := home.Sub(filepath.Dir(name))
		if err != nil {
			return err
		}
		actual, err := safefs.ReadFile(parent, filepath.Base(name), 8<<20)
		if errors.Is(err, os.ErrNotExist) && !change.Existed {
			parent.Close()
			continue
		}
		if err != nil {
			parent.Close()
			return err
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(actual))
		if hash != change.AfterHash && (!change.Existed || !bytesEqual(actual, change.Before)) {
			parent.Close()
			return fmt.Errorf("setup rollback found an unrelated file change: %s", name)
		}
		if change.Existed {
			err = safefs.ReplaceFile(parent, filepath.Base(name), actual, change.Before, 0600)
		} else {
			err = unix.Unlinkat(int(parent.File().Fd()), filepath.Base(name), 0)
			if err == nil {
				err = parent.File().Sync()
			}
		}
		parent.Close()
		if err != nil {
			return err
		}
	}
	journal.Files = map[string]setupFileChange{}
	return nil
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

func installSetupFile(home, stateRoot *safefs.Directory, journal *WorkerSetupJournal, path string, body []byte) error {
	parent, err := home.Sub(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	old, err := safefs.ReadFile(parent, filepath.Base(path), 8<<20)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if exists {
		expected, owned := journal.OwnedFiles[path]
		if !owned || fmt.Sprintf("%x", sha256.Sum256(old)) != expected {
			return fmt.Errorf("refusing to replace unowned or changed worker file: %s", path)
		}
	}

	journal.Files[path] = setupFileChange{Before: old, Existed: exists, AfterHash: fmt.Sprintf("%x", sha256.Sum256(body))}
	if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
		return err
	}
	return safefs.ReplaceFile(parent, filepath.Base(path), old, body, 0600)
}

func finishWorkerSetup(home string, root, stateRoot *safefs.Directory, journal *WorkerSetupJournal, request WorkerSetupRequest, archive *backupArchive) error {
	if err := requireWorkerSetupOwnership(home, journal); err != nil {
		return err
	}
	c := WorkerInstallation{Image: request.Image, RelayImage: request.RelayImage, Schema: 1, WorkerID: journal.WorkerID, ArchiveID: journal.ArchiveID, Policy: request.Policy, Qualification: map[string]bool{}}
	for _, source := range request.Sources {
		enrolled, ok := journal.SourceEnrollments[source.Alias]
		if !ok {
			return fmt.Errorf("source enrollment not recorded: %s", source.Alias)
		}
		c.Sources = append(c.Sources, deploysource.WorkerSource{Alias: source.Alias, Host: source.Host, User: source.User, Port: source.Port, KeyFile: filepath.Join(home, ".config/deploy/backup-worker/ssh", source.Alias+".key"), KnownHosts: filepath.Join(home, ".config/deploy/backup-worker/ssh", source.Alias+".known_hosts"), SourceID: enrolled.SourceID})
	}
	if len(c.Sources) != len(c.Policy.Sources) {
		return fmt.Errorf("source set differs from policy")
	}
	body, _ := json.Marshal(c)
	c.ConfigHash = fmt.Sprintf("%x", sha256.Sum256(body))
	for _, source := range c.Sources {
		source.KnownHosts = filepath.Join(home, stagedWorkerTrustPath(journal.ID, source.Alias))
		if err := deploysource.QualifyRestrictedSource(source, c.Policy.Timeouts); err != nil {
			return fmt.Errorf("restricted source qualification failed for %s: %w", source.Alias, err)
		}
	}
	c.Qualification["source_restrictions"] = true
	for _, image := range []string{request.Image, request.RelayImage} {
		if image == "" {
			continue
		}
		if _, err := process.LimitedOutput(exec.Command("podman", "pull", "--quiet", image), 15*time.Minute, 64<<10); err != nil {
			return fmt.Errorf("pinned runtime image pull failed")
		}
	}
	probe := exec.Command("podman", "run", "--rm", "--pull=never", "--network=none", "--cap-drop=all", "--security-opt=no-new-privileges", "--security-opt=seccomp=unconfined", "--read-only", "--tmpfs=/tmp:rw,size=128m", "--memory=512m", "--pids-limit=256", "--entrypoint=/usr/local/bin/deploy", request.Image, "__worker_selftest")
	proof, err := process.LimitedOutput(probe, 2*time.Minute, 4096)
	if err != nil {
		return fmt.Errorf("rootless image/SQLite/sandbox self-test failed")
	}
	var result workerSelfTestResult
	if err = protocol.StrictBackupJSON(proof, &result); err != nil || result.Architecture != runtime.GOARCH || !result.Isolated {
		return fmt.Errorf("runtime image returned incompatible qualification evidence")
	}
	binary, e := os.Open("/proc/self/exe")
	if e != nil {
		return e
	}
	digest, e := safefs.DigestFile(binary)
	binary.Close()
	if e != nil || digest != result.HelperSHA256 {
		return fmt.Errorf("worker image and helper artifacts must come from the same build")
	}
	if err = notification.ValidateMonitoringChecks(c.Policy.Monitoring); err != nil {
		return err
	}
	c.Qualification["rootless_runtime"] = true
	mountPolicy := c.Policy
	if journal.Shared != nil {
		mountPolicy.Notifications = config.WorkerNotifications{}
	}
	for file := range notificationMounts(mountPolicy) {
		if _, err = notification.ReadWorkerSecret(file); err != nil {
			return fmt.Errorf("notification/monitoring secret unavailable: configure private worker-host files")
		}
	}
	for _, name := range []string{".config/containers/systemd", ".config/systemd/user", ".local/share/deploy/backup-worker/signal", ".local/state/deploy/backup-worker/relay"} {
		if err = root.Mkdir(name); err != nil {
			return err
		}
	}
	paths := WorkerUnitPaths{Home: home, Config: filepath.Join(home, ".config/deploy/backup-worker/runtime.json"), State: filepath.Join(home, ".local/state/deploy/backup-worker/state"), Archive: c.Policy.Storage.Path, Keys: filepath.Join(home, ".config/deploy/backup-worker/ssh"), RelayState: filepath.Join(home, ".local/share/deploy/backup-worker/signal"), RelaySocket: filepath.Join(home, ".local/state/deploy/backup-worker/relay")}
	units, err := workerSetupUnits(c, paths, journal)
	if err != nil {
		return err
	}
	stage := filepath.Join(".local/state/deploy/backup-worker", journal.ID, "quadlets")
	if err = root.Mkdir(stage); err != nil {
		return err
	}
	staging, err := root.Sub(stage)
	if err != nil {
		return err
	}
	defer staging.Close()
	for name, body := range units {
		if !strings.HasSuffix(name, ".container") {
			continue
		}
		old, err := safefs.ReadFile(staging, name, 1<<20)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = safefs.ReplaceFile(staging, name, old, []byte(body), 0600); err != nil {
			return err
		}
	}
	generator := "/usr/lib/systemd/system-generators/podman-system-generator"
	if _, err = os.Stat(generator); err != nil {
		generator = "/usr/libexec/podman/quadlet"
	}
	generate := exec.Command(generator, "--user", "--dryrun")
	generate.Env = append(deploysource.SourceProcessEnvironment(home), "QUADLET_UNIT_DIRS="+filepath.Join(home, stage))
	generated, err := process.LimitedOutput(generate, 30*time.Second, 4<<20)
	if err != nil {
		return fmt.Errorf("installed Quadlet generator rejected worker units")
	}
	for name := range units {
		if strings.HasSuffix(name, ".container") && !strings.Contains(string(generated), "---"+strings.TrimSuffix(name, ".container")+".service---") {
			return fmt.Errorf("installed generator omitted %s", name)
		}
	}
	if !journal.StateInitialized && !journal.StateInitializing {
		if _, e := os.Lstat(paths.State); !errors.Is(e, os.ErrNotExist) {
			return fmt.Errorf("refusing to initialize unowned existing state directory")
		}
		journal.StateInitializing = true
		if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
			return err
		}
	}
	var state *backupState
	if journal.StateInitializing {
		state, err = openBackupStateWithLease(paths.State, true, true, true)
	} else {
		state, err = openBackupStateWithLease(paths.State, false, false, true)
	}
	if err != nil {
		return err
	}
	journal.StateInitialized = true
	journal.StateInitializing = false
	if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
		state.Close()
		return err
	}
	defer state.Close()
	if err = requireWorkerSetupOwner(state, journal); err != nil {
		return err
	}
	if err = installRecoverySupervisor(home, root, stateRoot, journal); err != nil {
		return err
	}
	if request.Recover {
		if err = archive.claimRecoveredWriter(state, c); err != nil {
			return err
		}
	}
	if err = prepareWorkerRecovery(root, stateRoot, journal, state, archive); err != nil {
		return err
	}
	var releaseInstallation func()
	success := false
	defer func() {
		if !success && releaseInstallation != nil {
			rollbackErr := rollbackWorkerInstallation(root, stateRoot, journal, state, archive)
			releaseInstallation()
			releaseInstallation = nil
			if rollbackErr == nil {
				_ = finishWorkerRollback(stateRoot, journal, state)
			}
		}
		if releaseInstallation != nil {
			releaseInstallation()
		}
		// If draining or lock acquisition failed, the independent recovery timer
		// retries after the live coordinator exits. The journal remains armed.
	}()
	if journal.Updating {
		for _, unit := range journal.EnabledUnits {
			if _, err = process.LimitedOutput(exec.Command("systemctl", "--user", "disable", unit), 10*time.Second, 4096); err != nil {
				return err
			}
		}
		if err = stopWorkerUnitsFor(journal.Shared != nil); err != nil {
			return err
		}
	}
	releaseInstallation, err = lockWorkerInstallation(state, archive)
	if err != nil {
		return err
	}
	var control ArchiveControl
	err = state.get("control", &control)
	if err == sql.ErrNoRows {
		control = ArchiveControl{Schema: 1, ArchiveID: c.ArchiveID, WriterID: c.WorkerID, Epoch: archive.identity.Epoch, Revision: 1, ConfigHash: c.ConfigHash, Instances: map[string]ExpectedInstance{}, RetiredSources: map[string]string{}, Pins: map[string]string{}, Retention: c.Policy.Retention}
		if err = state.put("control", control); err != nil {
			return err
		}
		if err = state.put("mode", "writer"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if control.ConfigHash != c.ConfigHash {
		if !journal.Updating {
			return fmt.Errorf("existing state configuration requires explicit update reconciliation")
		}
		if journal.BeforeControl == nil {
			copy := control
			journal.BeforeControl = &copy
			if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
				return err
			}
		}
	}
	// A crash after creating control but before mode is a journal-owned bootstrap.
	var mode string
	if e := state.get("mode", &mode); e == sql.ErrNoRows {
		if err = state.put("mode", "writer"); err != nil {
			return err
		}
	} else if e != nil || mode != "writer" {
		return fmt.Errorf("setup cannot replace recovery/maintenance state")
	}

	if err = state.put("min_free_bytes", c.Policy.Storage.MinFreeBytes); err != nil {
		return err
	}
	if err = archive.commitControl(state); err != nil {
		return err
	}
	journal.Stage = "installing"
	if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
		return err
	}

	if journal.Updating {
		if err = archive.updateControl(state, func(current *ArchiveControl) error {
			current.ConfigHash = c.ConfigHash
			current.Retention = c.Policy.Retention
			return nil
		}); err != nil {
			return err
		}
	}
	if err = installWorkerTrust(root, stateRoot, journal, request.Sources); err != nil {
		return err
	}
	containerConfig := c
	containerConfig.Policy = containerNotificationPolicy(c.Policy)
	containerConfig.Sources = append([]deploysource.WorkerSource(nil), c.Sources...)
	for i := range containerConfig.Sources {
		containerConfig.Sources[i].KeyFile = "/keys/" + c.Sources[i].Alias + ".key"
		containerConfig.Sources[i].KnownHosts = "/keys/" + c.Sources[i].Alias + ".known_hosts"
	}
	for name, value := range map[string]any{"runtime.json": c, "container.json": containerConfig, "images.json": map[string]string{"worker": request.Image, "relay": request.RelayImage}} {
		body, _ := json.Marshal(value)
		if err = installSetupFile(root, stateRoot, journal, ".config/deploy/backup-worker/"+name, body); err != nil {
			return err
		}
	}
	for name, body := range units {
		directory := ".config/systemd/user"
		if strings.HasSuffix(name, ".container") {
			directory = ".config/containers/systemd"
		}
		if err = installSetupFile(root, stateRoot, journal, filepath.Join(directory, name), []byte(body)); err != nil {
			return err
		}
	}
	if err = helper.InstallSelf(home, "backup-worker"); err != nil {
		return err
	}
	if _, err = process.LimitedOutput(exec.Command("systemctl", "--user", "daemon-reload"), 15*time.Second, 4096); err != nil {
		return err
	}
	// Stage timers, but do not claim complete protection before the deferred live
	// qualification is acknowledged. Manual run/pull/status are already usable.
	// Preserve rollback before-images until the completion record is durable.
	if err = state.put("installation", "staged-awaiting-qualification"); err != nil {
		return err
	}
	completed := *journal
	completed.Stage = "complete"
	completed.RecoveryNeeded = false
	completed.BeforeHelper = nil
	completed.BeforeInstallation = ""
	completed.CandidateHelper = ""
	completed.Updating = false
	completed.BeforeControl = nil
	completed.BeforeMinFreeBytes = nil
	completed.PreviousRequestHash = ""
	completed.EnabledUnits = nil
	completed.OwnedFiles = map[string]string{}
	for path, hash := range journal.OwnedFiles {
		completed.OwnedFiles[path] = hash
	}
	for path, change := range journal.Files {
		completed.OwnedFiles[path] = change.AfterHash
	}
	completed.Files = map[string]setupFileChange{}
	if err = stateRoot.WriteJSON("setup.json", completed, true); err != nil {
		return err
	}
	*journal = completed
	success = true
	if err = clearInstallationGate(state.directory, journal.ID); err != nil {
		return err
	}
	return nil
}

type workerSelfTestResult struct {
	Schema        int    `json:"schema"`
	Architecture  string `json:"architecture"`
	Isolated      bool   `json:"isolated"`
	SQLiteVersion string `json:"sqlite_version"`
	HelperSHA256  string `json:"helper_sha256"`
}

func WorkerSelfTest() error {
	dir, err := os.MkdirTemp("/tmp", "deploy-worker-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		return err
	}
	_, err = db.Exec("CREATE TABLE qualification(value TEXT NOT NULL); INSERT INTO qualification VALUES('standalone');")
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	snapshot, err := os.Open(file)
	if err != nil {
		return err
	}
	defer snapshot.Close()
	validation, err := isolatedSnapshotValidation(snapshot, true, time.Minute)
	if err != nil {
		return err
	}
	binary, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	defer binary.Close()
	digest, err := safefs.DigestFile(binary)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(workerSelfTestResult{1, runtime.GOARCH, true, validation.SQLiteVersion, digest})
}
