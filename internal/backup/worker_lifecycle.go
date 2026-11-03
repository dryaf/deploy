package backup

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/transport"
	"github.com/dryaf/deploy/internal/ui"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

type workerLifecycleRequest struct {
	Operation string                                `json:"operation"`
	Source    string                                `json:"source,omitempty"`
	Results   []deploysource.SourceEnrollmentResult `json:"results,omitempty"`
}

type workerLifecycleJournal struct {
	Schema       int                       `json:"schema"`
	Operation    string                    `json:"operation"`
	Stage        string                    `json:"stage"`
	Installation WorkerInstallation        `json:"installation"`
	Source       deploysource.WorkerSource `json:"source"`
	Previous     string                    `json:"previous"`
	PublicKey    string                    `json:"public_key"`
	EnabledUnits []string                  `json:"enabled_units"`
}

func lifecycleRemote(server config.ServerConfig, request workerLifecycleRequest) (workerLifecycleJournal, error) {
	var result workerLifecycleJournal
	body, _ := json.Marshal(request)
	cmd := workerAdminCommand(server, []string{"__lifecycle"})
	cmd.Stdin = strings.NewReader(string(body))
	out, err := process.LimitedOutput(cmd, 5*time.Minute, safefs.MaxRecordBytes)
	if err != nil {
		return result, err
	}
	err = safefs.DecodeJSON(out, &result, safefs.MaxRecordBytes)
	return result, err
}

func lifecycleWorkerCLI(operation string, args []string) error {
	flags := flag.NewFlagSet("backup-worker "+operation, flag.ContinueOnError)
	source := flags.String("source", "", "Configured source alias for rotation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || operation == "rotate-key" && *source == "" {
		return fmt.Errorf("select a worker; rotate-key also requires --source")
	}
	inventory, err := config.ReadInventory(ui.InventoryPath(""))
	if err != nil {
		return err
	}
	worker, ok := inventory.Servers[flags.Arg(0)]
	if !ok || worker.BackupWorker == nil {
		return fmt.Errorf("unknown backup worker")
	}
	if ui.DryRun {
		fmt.Printf("[DRY] %s worker %s; archives retained; no SSH or mutation.\n", operation, flags.Arg(0))
		return nil
	}
	journal, err := lifecycleRemote(worker, workerLifecycleRequest{Operation: operation + "-prepare", Source: *source})
	if err != nil {
		return err
	}
	if journal.Stage == "complete" && operation == "uninstall" {
		return nil
	}
	sources := journal.Installation.Sources
	if operation == "rotate-key" {
		sources = []deploysource.WorkerSource{journal.Source}
	}
	var results []deploysource.SourceEnrollmentResult
	for _, src := range sources {
		admin, exists := inventory.Servers[src.Alias]
		if !exists || admin.Host != src.Host || admin.User != src.User {
			return fmt.Errorf("source administrator inventory changed; reconcile before lifecycle mutation")
		}
		request := deploysource.SourceEnrollmentRequest{Schema: 1, Operation: "revoke", WorkerID: journal.Installation.WorkerID, SourceID: src.SourceID}
		if operation == "rotate-key" {
			request.Operation = "rotate"
			request.PublicKey = journal.PublicKey
			request.Previous = journal.Previous
		}
		body, _ := json.Marshal(request)
		cmd := transport.WorkerAdminShell(admin, `exec "$HOME/.local/lib/deploy/backup-source/deploy" __source_admin`)
		cmd.Stdin = strings.NewReader(string(body))
		out, e := process.LimitedOutput(cmd, 2*time.Minute, protocol.MaxBackupHeader)
		if e != nil {
			return e
		}
		var result deploysource.SourceEnrollmentResult
		if e = protocol.StrictBackupJSON(out, &result); e != nil || result.SourceID != src.SourceID || result.WorkerID != journal.Installation.WorkerID {
			return fmt.Errorf("source lifecycle acknowledgement mismatch")
		}
		results = append(results, result)
	}
	_, err = lifecycleRemote(worker, workerLifecycleRequest{Operation: operation + "-finish", Source: *source, Results: results})
	return err
}

func privateKeyPublic(dir *safefs.Directory, name string) ([]byte, string, error) {
	body, err := safefs.ReadFile(dir, name, 8192)
	if err != nil {
		return nil, "", err
	}
	key, err := ssh.ParsePrivateKey(body)
	if err != nil {
		return nil, "", fmt.Errorf("invalid owned private key")
	}
	return body, string(ssh.MarshalAuthorizedKey(key.PublicKey())), nil
}

func runWorkerLifecycle() error {
	var request workerLifecycleRequest
	if err := safefs.ReadJSON(os.Stdin, &request, protocol.MaxBackupHeader); err != nil {
		return err
	}
	home, err := deploysource.TrustedSourceHome()
	if err != nil {
		return err
	}
	return workerLifecycle(home, request)
}

func workerLifecycle(home string, request workerLifecycleRequest) error {
	operation, stage, ok := strings.Cut(request.Operation, "-")
	if operation == "rotate" {
		operation = "rotate-key"
		stage = strings.TrimPrefix(request.Operation, "rotate-key-")
	}
	if !ok || (operation != "rotate-key" && operation != "uninstall") || (stage != "prepare" && stage != "finish") {
		return fmt.Errorf("unsupported lifecycle operation")
	}
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	coordinator, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	if err != nil {
		return err
	}
	defer coordinator.Close()
	dir, err := root.Sub(".local/state/deploy/backup-worker")
	if err != nil {
		return err
	}
	defer dir.Close()
	var journal workerLifecycleJournal
	readErr := dir.ReadJSON("lifecycle.json", &journal)
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	if readErr == nil && journal.Stage != "complete" && (journal.Operation != operation || operation == "rotate-key" && journal.Source.Alias != request.Source) {
		return fmt.Errorf("another lifecycle operation requires completion")
	}
	if stage == "prepare" && (os.IsNotExist(readErr) || journal.Stage == "complete" && (operation == "rotate-key" || journal.Operation != operation)) {
		c, e := loadWorkerInstallation(filepath.Join(home, ".config/deploy/backup-worker/runtime.json"))
		if e != nil {
			return e
		}
		journal = workerLifecycleJournal{Schema: 1, Operation: operation, Stage: "preparing", Installation: c, EnabledUnits: enabledWorkerUnits()}
		if operation == "rotate-key" {
			for _, src := range c.Sources {
				if src.Alias == request.Source {
					journal.Source = src
				}
			}
			if journal.Source.Alias == "" {
				return fmt.Errorf("unknown source alias")
			}
			keys, e := safefs.Open(filepath.Dir(journal.Source.KeyFile))
			if e != nil {
				return e
			}
			_, public, e := privateKeyPublic(keys, filepath.Base(journal.Source.KeyFile))
			keys.Close()
			if e != nil {
				return e
			}
			key, _, _, _, e := ssh.ParseAuthorizedKey([]byte(public))
			if e != nil {
				return e
			}
			journal.Previous = ssh.FingerprintSHA256(key)
		}
		if err = dir.WriteJSON("lifecycle.json", journal, !os.IsNotExist(readErr)); err != nil {
			return err
		}
	}
	if journal.Schema != 1 || journal.Operation != operation {
		return fmt.Errorf("lifecycle journal does not match request")
	}
	if journal.Stage == "complete" {
		return json.NewEncoder(os.Stdout).Encode(journal)
	}
	state, err := openBackupState(filepath.Join(home, ".local/state/deploy/backup-worker/state"), false)
	if err != nil {
		return err
	}
	defer state.Close()
	if err = state.put("mode", "maintenance"); err != nil {
		return err
	}
	if operation == "uninstall" {
		// A retry may follow removal of the already-disabled supervisor unit.
		if _, e := safefs.ReadFile(root, ".config/systemd/user/"+recoveryTimer, 8<<20); e == nil {
			if _, err = process.LimitedOutput(exec.Command("systemctl", "--user", "disable", "--now", recoveryTimer), 15*time.Second, 4096); err != nil {
				return err
			}
		} else if !os.IsNotExist(e) {
			return e
		}
		if err = stopWorkerUnits(); err != nil {
			return err
		}
	} else {
		if _, err = process.LimitedOutput(exec.Command("systemctl", "--user", "stop", "deploy-backup.timer", "deploy-backup.service"), 2*time.Minute, 4096); err != nil {
			return err
		}
	}
	if stage == "prepare" {
		if operation == "rotate-key" && journal.Stage == "preparing" {
			keys, e := safefs.Open(filepath.Dir(journal.Source.KeyFile))
			if e != nil {
				return e
			}
			defer keys.Close()
			name := filepath.Base(journal.Source.KeyFile) + ".next"
			_, public, e := privateKeyPublic(keys, name)
			if os.IsNotExist(e) {
				_, private, e := ed25519.GenerateKey(rand.Reader)
				if e != nil {
					return e
				}
				block, e := ssh.MarshalPrivateKey(private, "deploy backup worker rotation")
				if e != nil {
					return e
				}
				if e = safefs.ReplaceFile(keys, name, nil, pem.EncodeToMemory(block), 0600); e != nil {
					return e
				}
				_, public, e = privateKeyPublic(keys, name)
				if e != nil {
					return e
				}
			} else if e != nil {
				return e
			}
			journal.PublicKey = public
		}
		journal.Stage = "prepared"
		if err = dir.WriteJSON("lifecycle.json", journal, true); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(journal)
	}
	if journal.Stage != "prepared" {
		return fmt.Errorf("lifecycle preparation is incomplete")
	}
	if operation == "rotate-key" {
		key, _, _, _, e := ssh.ParseAuthorizedKey([]byte(journal.PublicKey))
		if e != nil {
			return e
		}
		fingerprint := ssh.FingerprintSHA256(key)
		if len(request.Results) != 1 || request.Results[0].SourceID != journal.Source.SourceID || request.Results[0].WorkerID != journal.Installation.WorkerID || request.Results[0].Fingerprint != fingerprint {
			return fmt.Errorf("rotation acknowledgement differs from prepared key")
		}
		keys, e := safefs.Open(filepath.Dir(journal.Source.KeyFile))
		if e != nil {
			return e
		}
		defer keys.Close()
		name := filepath.Base(journal.Source.KeyFile)
		current, currentPublic, e := privateKeyPublic(keys, name)
		if e != nil {
			return e
		}
		currentKey, _, _, _, e := ssh.ParseAuthorizedKey([]byte(currentPublic))
		if e != nil {
			return e
		}
		currentFingerprint := ssh.FingerprintSHA256(currentKey)
		if currentFingerprint != fingerprint {
			if currentFingerprint != journal.Previous {
				return fmt.Errorf("worker key changed outside rotation")
			}
			next, public, e := privateKeyPublic(keys, name+".next")
			if e != nil || public != journal.PublicKey {
				return fmt.Errorf("prepared private key unavailable")
			}
			if e = safefs.ReplaceFile(keys, name, current, next, 0600); e != nil {
				return e
			}
		}
		if err = deploysource.QualifyRestrictedSource(journal.Source, journal.Installation.Policy.Timeouts); err != nil {
			return err
		}
		if err = unix.Unlinkat(int(keys.File().Fd()), name+".next", 0); err != nil && err != unix.ENOENT {
			return err
		}
		if err = keys.File().Sync(); err != nil {
			return err
		}
		var setup WorkerSetupJournal
		if err = dir.ReadJSON("setup.json", &setup); err != nil {
			return err
		}
		setup.SourceEnrollments[journal.Source.Alias] = request.Results[0]
		if err = dir.WriteJSON("setup.json", setup, true); err != nil {
			return err
		}
		if err = state.put("mode", "writer"); err != nil {
			return err
		}
		for _, unit := range journal.EnabledUnits {
			if unit == "deploy-backup.timer" {
				if _, err = process.LimitedOutput(exec.Command("systemctl", "--user", "start", unit), 10*time.Second, 4096); err != nil {
					return err
				}
			}
		}
	} else {
		for _, source := range journal.Installation.Sources {
			found := false
			for _, result := range request.Results {
				found = found || result.SourceID == source.SourceID && result.WorkerID == journal.Installation.WorkerID && result.Fingerprint == ""
			}
			if !found {
				return fmt.Errorf("source revocation acknowledgement missing")
			}
		}
		var setup WorkerSetupJournal
		if err = dir.ReadJSON("setup.json", &setup); err != nil {
			return err
		}
		// Validate the complete ownership set before deleting anything. Interrupted
		// removals may be absent, but a changed or foreign file is always preserved.
		for name, hash := range setup.OwnedFiles {
			body, e := safefs.ReadFile(root, name, 8<<20)
			if os.IsNotExist(e) {
				continue
			}
			if e != nil || fmt.Sprintf("%x", sha256.Sum256(body)) != hash {
				return fmt.Errorf("uninstall found changed owned file: %s", name)
			}
		}
		for _, unit := range journal.EnabledUnits {
			if _, err = process.LimitedOutput(exec.Command("systemctl", "--user", "disable", unit), 10*time.Second, 4096); err != nil {
				return err
			}
		}
		for name := range setup.OwnedFiles {
			parent, e := root.Sub(filepath.Dir(name))
			if e != nil {
				return e
			}
			e = unix.Unlinkat(int(parent.File().Fd()), filepath.Base(name), 0)
			if e == nil {
				e = parent.File().Sync()
			}
			parent.Close()
			if e != nil && e != unix.ENOENT {
				return e
			}
		}
		setup.Stage = "uninstalled"
		if err = dir.WriteJSON("setup.json", setup, true); err != nil {
			return err
		}
		if _, err = process.LimitedOutput(exec.Command("systemctl", "--user", "daemon-reload"), 10*time.Second, 4096); err != nil {
			return err
		}
		if err = state.put("mode", "read"); err != nil {
			return err
		}
		if err = state.put("installation", "uninstalled-archive-retained"); err != nil {
			return err
		}
	}
	journal.Stage = "complete"
	if err = dir.WriteJSON("lifecycle.json", journal, true); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(journal)
}
