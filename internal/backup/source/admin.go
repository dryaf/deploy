package source

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	deployhelper "github.com/dryaf/deploy/internal/backup/helper"
	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

type SourceEnrollmentRequest struct {
	HelperSHA256 string `json:"helper_sha256,omitempty"`
	SourceID     string `json:"source_id,omitempty"`
	Previous     string `json:"previous,omitempty"`
	Schema       int    `json:"schema"`
	Operation    string `json:"operation"`
	WorkerID     string `json:"worker_id"`
	PublicKey    string `json:"public_key"`
	Service      string `json:"service,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type SourceEnrollmentResult struct {
	Schema      int    `json:"schema"`
	SourceID    string `json:"source_id"`
	WorkerID    string `json:"worker_id"`
	Fingerprint string `json:"fingerprint"`
}

func Admin() error {
	home, err := TrustedSourceHome()
	if err != nil {
		return err
	}
	var request SourceEnrollmentRequest
	if err = safefs.ReadJSON(os.Stdin, &request, protocol.MaxBackupHeader); err != nil {
		return err
	}
	if request.Operation == "inspect" {
		result, err := inspectSourceEnrollment(home, request)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if request.Operation == "revoke-retained" {
		result, err := revokeRetainedSource(home, request)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if request.Operation == "reconcile-generation" {
		result, e := reconcileSourceGeneration(home, request)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if request.Operation == "enroll" || request.Operation == "rotate" {
		if err = sourceEnrollmentPreflight(home); err != nil {
			return err
		}
	}
	result, err := enrollSourceWorker(home, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func enrollSourceWorker(home string, request SourceEnrollmentRequest) (SourceEnrollmentResult, error) {
	var result SourceEnrollmentResult
	if request.Schema != 1 || !safefs.ValidID(request.WorkerID, "w") || (request.Operation != "enroll" && request.Operation != "rotate" && request.Operation != "revoke") {
		return result, fmt.Errorf("invalid source enrollment operation")
	}
	var public ssh.PublicKey
	var err error
	if request.Operation == "enroll" || request.Operation == "rotate" {
		var options []string
		var rest []byte
		public, _, options, rest, err = ssh.ParseAuthorizedKey([]byte(request.PublicKey))
		if err != nil || public.Type() != ssh.KeyAlgoED25519 || len(options) > 0 || len(bytes.TrimSpace(rest)) > 0 {
			return result, fmt.Errorf("enrollment requires exactly one bare Ed25519 public key")
		}
	}
	homeDir, err := safefs.Open(home)
	if err != nil {
		return result, err
	}
	defer homeDir.Close()
	for _, name := range []string{".local/state/deploy/backup-source/generations", ".local/state/deploy/backup-source/exports", ".ssh"} {
		if err = homeDir.Mkdir(name); err != nil {
			return result, err
		}
	}
	dir, err := homeDir.Sub(".local/state/deploy/backup-source")
	if err != nil {
		return result, err
	}
	defer dir.Close()
	lock, err := dir.Lock("policy.lock", true, true)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	// A declaration must not regain retired access by naming the same source
	// under another alias. Ordinary enrollment/rotation has no authority to erase
	// the explicit disposition, even when the selected key is unchanged.
	if request.Operation == "enroll" || request.Operation == "rotate" {
		var retained SourceIdentity
		if e := dir.ReadJSON("identity.json", &retained); e == nil {
			if retained.Workers[request.WorkerID].RetirementReason != "" {
				return result, fmt.Errorf("worker authorization is retired; explicit source reenrollment migration required")
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return result, e
		}
	}
	if err = deployhelper.InstallSelf(home, "backup-source"); err != nil {
		return result, err
	}
	for _, name := range []string{"export.lock", "session.lock"} {
		l, e := dir.Lock(name, true, true)
		if e != nil {
			return result, e
		}
		l.Close()
	}
	var identity SourceIdentity
	err = dir.ReadJSON("identity.json", &identity)
	if errors.Is(err, os.ErrNotExist) {
		// Existing generation records are never silently adopted after identity loss.
		generations, e := dir.Sub("generations")
		if e != nil {
			return result, e
		}
		entries, e := generations.File().ReadDir(1)
		generations.Close()
		if e != nil && e != io.EOF {
			return result, e
		}
		if len(entries) > 0 {
			return result, fmt.Errorf("source identity missing with existing generations; recover identity explicitly")
		}
		id, e := safefs.NewID("s")
		if e != nil {
			return result, e
		}
		identity = SourceIdentity{1, id, map[string]SourceAuthorization{}, map[string]string{}}
		if e = dir.WriteJSON("identity.json", identity, false); e != nil {
			return result, e
		}
	} else if err != nil {
		return result, err
	}
	if request.SourceID != "" && request.SourceID != identity.ID {
		return result, fmt.Errorf("source identity does not match enrollment request")
	}
	if identity.Schema != 1 || !safefs.ValidID(identity.ID, "s") {
		return result, fmt.Errorf("invalid source identity")
	}
	if identity.Workers == nil {
		identity.Workers = map[string]SourceAuthorization{}
	}
	var rate SourceRateLedger
	if err = dir.ReadJSON("rate.json", &rate); errors.Is(err, os.ErrNotExist) {
		boot, uptime, e := process.HostClock()
		if e != nil {
			return result, e
		}
		if e = dir.WriteJSON("rate.json", SourceRateLedger{1, identity.ID, boot, uptime, map[string]SourceRateUsage{}}, false); e != nil {
			return result, e
		}
	} else if err != nil {
		return result, err
	}
	if _, known := identity.Workers[request.WorkerID]; !known && len(identity.Workers) >= 64 {
		return result, fmt.Errorf("source enrollment limit reached")
	}
	knownAuthorization := identity.Workers[request.WorkerID]
	result = SourceEnrollmentResult{Schema: 1, SourceID: identity.ID, WorkerID: request.WorkerID}
	// Policy is installed before a new key becomes usable and revoked before key
	// removal. A crash in either direction leaves the narrower authorization.
	if request.Operation == "enroll" || request.Operation == "rotate" {
		result.Fingerprint = ssh.FingerprintSHA256(public)
		known := identity.Workers[request.WorkerID]
		if known.Enabled && known.Fingerprint != "" && known.Fingerprint != result.Fingerprint && (request.Operation != "rotate" || known.Fingerprint != request.Previous) {
			return result, fmt.Errorf("existing worker key differs; revoke explicitly before enrolling a replacement")
		}
		previous := known.Previous
		if known.Fingerprint != result.Fingerprint {
			previous = known.Fingerprint
		}
		identity.Workers[request.WorkerID] = SourceAuthorization{Enabled: true, Fingerprint: result.Fingerprint, Previous: previous}
	} else {
		knownAuthorization.Enabled = false
		identity.Workers[request.WorkerID] = knownAuthorization
	}
	if err = dir.WriteJSON("identity.json", identity, true); err != nil {
		return result, err
	}
	sshDir, err := homeDir.Sub(".ssh")
	if err != nil {
		return result, err
	}
	defer sshDir.Close()
	info, err := sshDir.File().Stat()
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return result, fmt.Errorf("source .ssh must be private (0700)")
	}
	var old []byte
	file, err := sshDir.Open("authorized_keys", unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err == nil {
		info, e := file.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			file.Close()
			return result, fmt.Errorf("unsafe authorized_keys permissions/type")
		}
		old, err = io.ReadAll(io.LimitReader(file, 1<<20+1))
		file.Close()
		if err != nil {
			return result, err
		}
		if len(old) > 1<<20 {
			return result, fmt.Errorf("authorized_keys exceeds enrollment limit")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	marker := "deploy-backup:" + request.WorkerID
	var lines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(old), "\n"), "\n") {
		if strings.Contains(line, marker) {
			oldPublic, comment, options, _, err := ssh.ParseAuthorizedKey([]byte(line))
			owned := err == nil && comment == marker
			oldFingerprint := ""
			if oldPublic != nil {
				oldFingerprint = ssh.FingerprintSHA256(oldPublic)
			}
			forced := "command=\"" + filepath.Join(home, ".local/lib/deploy/backup-source/deploy") + " source-serve --worker-id " + request.WorkerID + " --key-id " + oldFingerprint + "\""
			hasForced, hasRestrict := false, false
			for _, option := range options {
				hasForced = hasForced || option == forced
				hasRestrict = hasRestrict || option == "restrict"
			}
			authorized := identity.Workers[request.WorkerID]
			if !owned || !hasForced || !hasRestrict || (oldFingerprint != authorized.Fingerprint && oldFingerprint != authorized.Previous) {
				return result, fmt.Errorf("conflicting unowned authorized_keys entry")
			}
			continue
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	if request.Operation == "enroll" || request.Operation == "rotate" {
		helper := filepath.Join(home, ".local/lib/deploy/backup-source/deploy")
		if strings.ContainsAny(helper, "\"\\\r\n \t;$`&|()<>") {
			return result, fmt.Errorf("unsupported helper path")
		}
		line := "restrict,command=\"" + helper + " source-serve --worker-id " + request.WorkerID + " --key-id " + result.Fingerprint + "\" " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public))) + " " + marker
		lines = append(lines, line)
	}
	next := []byte(strings.Join(lines, "\n") + "\n")
	return result, safefs.ReplaceFile(sshDir, "authorized_keys", old, next, 0600)
}

func sourceEnrollmentPreflight(home string) error {
	for _, name := range []string{"podman", "systemctl", "sqlite3", "prlimit", "timeout", "python3"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("source prerequisite missing: %s", name)
		}
	}
	probe := exec.Command("prlimit", "--as=268435456", "--cpu=3", "--", "timeout", "5s", "sqlite3", "-init", "/dev/null", "-batch", "-nofollow", ":memory:", "SELECT sqlite_version();")
	probe.Env = SourceProcessEnvironment(home)
	if _, err := process.LimitedOutput(probe, 6*time.Second, 1024); err != nil {
		return fmt.Errorf("source SQLite/watchdog profile is unsupported")
	}
	cmd := exec.Command("systemctl", "--user", "show-environment")
	cmd.Env = SourceProcessEnvironment(home)
	if _, err := process.LimitedOutput(cmd, 5*time.Second, 64<<10); err != nil {
		return fmt.Errorf("source user manager unavailable")
	}
	return nil
}
