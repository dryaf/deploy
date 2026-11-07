package backup

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

// A lost journal or lock cannot be interpreted as permission to reserve a new
// identity. Interrupted pre-reservation directories remain for explicit review.
func checkDeclarativeSetupReservation(root *safefs.Directory, desired ...*SetupDeclaration) error {
	exists := func(path string) (bool, error) {
		f, err := openBackupOwnedFile(root, path, os.O_RDONLY, 0)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		f.Close()
		return true, nil
	}
	lock, err := exists(".local/state/deploy/locks/backup-worker-setup.lock")
	if err != nil {
		return err
	}
	journal, err := exists(".local/state/deploy/backup-worker/setup.json")
	if err != nil {
		return err
	}
	if lock != journal {
		return fmt.Errorf("backup setup reservation is incomplete or lost; explicit recovery required")
	}
	if !journal {
		if len(desired) == 1 && desired[0] != nil && desired[0].Desired != nil && desired[0].Desired.Backup != nil && len(desired[0].Desired.Backup.RetireSources) != 0 {
			return fmt.Errorf("source retirement requires an existing declared installation")
		}
		paths := []string{".local/state/deploy/backup-worker/state", ".config/deploy/backup-worker/runtime.json", ".config/deploy/backup-worker/ssh", ".local/lib/deploy/backup-worker/deploy"}
		if len(desired) == 1 && desired[0] != nil && desired[0].Paths != nil {
			p, err := declarationPaths(root.File().Name(), desired[0])
			if err != nil {
				return err
			}
			paths = append(paths, p.State, p.Runtime, p.Keys(), p.Helper())
		}
		for _, path := range paths {
			present, err := exists(path)
			if err != nil {
				return err
			}
			if present {
				return fmt.Errorf("existing backup state has no setup reservation; explicit adoption or recovery required")
			}
		}
	}
	return nil
}

// SetupDeclaration pins the high-level coordinator's resolved identity and
// artifacts into the existing domain journal's request hash. Legacy callers omit
// it; they cannot accidentally adopt an installation created by this interface.
type SetupDeclaration struct {
	Paths                   *InstallationPaths     `json:"paths,omitempty"`
	Desired                 *config.DesiredWorkers `json:"desired,omitempty"`
	NotificationDesiredHash string                 `json:"notification_desired_hash,omitempty"`
	NotificationRoot        string                 `json:"notification_root,omitempty"`
	NotificationRoute       string                 `json:"notification_route,omitempty"`
	Version                 int                    `json:"version"`
	DesiredHash             string                 `json:"desired_hash"`
	HelperSHA256            string                 `json:"helper_sha256"`
	Sources                 map[string]string      `json:"sources"`
	UID                     int                    `json:"uid"`
	User                    string                 `json:"user"`
	Home                    string                 `json:"home"`
	Architecture            string                 `json:"architecture"`
}

func validateSetupDeclaration(r WorkerSetupRequest, home string) error {
	d := r.Declaration
	if d == nil {
		if r.Policy.Notifications.Mode == "shared" {
			return fmt.Errorf("fresh shared setup requires declarative ownership")
		}
		return nil
	}
	if err := validateSetupDesired(d); err != nil {
		return err
	}
	if d.NotificationDesiredHash != "" && (d.NotificationRoot == "" || !safefs.DigestPattern.MatchString(d.NotificationDesiredHash)) {
		return fmt.Errorf("invalid notification dependency policy scope")
	}
	if (r.Policy.Notifications.Mode == "shared") != (d.NotificationRoot != "") || d.NotificationRoot != "" && (!filepath.IsAbs(d.NotificationRoot) || filepath.Clean(d.NotificationRoot) != d.NotificationRoot || d.NotificationRoute == "") || d.NotificationRoot == "" && d.NotificationRoute != "" {
		return fmt.Errorf("invalid shared notification dependency pin")
	}
	if (d.Version != 1 && d.Version != 2) || !safefs.DigestPattern.MatchString(d.DesiredHash) || !safefs.DigestPattern.MatchString(d.HelperSHA256) || len(d.Sources) != len(r.Sources) {
		return fmt.Errorf("invalid declarative setup pins")
	}
	for _, s := range r.Sources {
		if !safefs.DigestPattern.MatchString(d.Sources[s.Alias]) {
			return fmt.Errorf("missing source helper pin")
		}
	}
	account, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || d.UID <= 0 || d.UID != os.Getuid() || d.User != account.Username || d.Home != home || d.Architecture != runtime.GOARCH || runtime.GOOS != "linux" {
		return fmt.Errorf("setup target identity changed since inspection")
	}
	if d.Version == 1 {
		for name, suffix := range map[string]string{"XDG_CONFIG_HOME": ".config", "XDG_STATE_HOME": ".local/state", "XDG_DATA_HOME": ".local/share"} {
			if value := os.Getenv(name); value != "" && value != filepath.Join(home, suffix) {
				return fmt.Errorf("backup target XDG layout changed; explicit path migration required")
			}
		}
	} else {
		current, e := ResolveInstallationPaths(home, os.Getenv("XDG_CONFIG_HOME"), os.Getenv("XDG_STATE_HOME"), os.Getenv("XDG_DATA_HOME"), d.Desired.Backup.Paths)
		if e != nil || !reflect.DeepEqual(current, *d.Paths) {
			return fmt.Errorf("backup target paths changed since inspection")
		}
		if _, e := current.UnitPaths(r.Policy.Storage.Path); e != nil {
			return e
		}
	}
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	defer f.Close()
	digest, err := safefs.DigestFile(f)
	if err != nil || digest != d.HelperSHA256 {
		return fmt.Errorf("setup helper differs from selected artifact")
	}
	return nil
}

// Older declarative journals did not retain a policy snapshot. They remain
// readable, but cannot provide a complete field diff until explicitly imported.
func validateSetupDesired(d *SetupDeclaration) error {
	if d == nil {
		return fmt.Errorf("backup declaration authority is missing")
	}
	if d.Version == 2 {
		if d.Paths == nil || d.Desired == nil || d.Paths.Home != d.Home || d.Desired.Backup == nil {
			return fmt.Errorf("backup path authority is missing")
		}
		if err := d.Paths.Validate(); err != nil {
			return err
		}
		if err := d.Paths.MatchesDesired(d.Desired.Backup.Paths); err != nil {
			return err
		}
	} else if d.Paths != nil {
		return fmt.Errorf("backup path authority requires declaration version 2")
	}
	if d.Desired == nil {
		return nil
	}
	hash, err := d.Desired.Fingerprint()
	if err != nil || hash != d.DesiredHash || d.Desired.Backup == nil {
		return fmt.Errorf("retained backup policy snapshot differs from its declaration pin")
	}
	if d.NotificationDesiredHash != "" {
		hash, err = d.Desired.NotificationFingerprint()
		if err != nil || hash != d.NotificationDesiredHash {
			return fmt.Errorf("retained backup dependency snapshot differs from its notification pin")
		}
	}
	return nil
}
