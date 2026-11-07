package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

func declarationPaths(home string, d *SetupDeclaration) (InstallationPaths, error) {
	if d == nil || d.Version == 1 && d.Paths == nil {
		return ResolveInstallationPaths(home, "", "", "", config.WorkerPaths{})
	}
	if d.Version != 2 || d.Paths == nil || d.Home != home || d.Paths.Home != home {
		return InstallationPaths{}, fmt.Errorf("backup installation path authority changed")
	}
	if err := validateSetupDesired(d); err != nil {
		return InstallationPaths{}, err
	}
	return *d.Paths, nil
}

// The fixed setup journal is a locator, never desired-policy fallback. Its path
// pins must agree with the runtime and with a versioned declaration. Callers that
// mutate state hold the existing setup lease while consulting it.
func installedBackupPaths(root *safefs.Directory) (InstallationPaths, *SetupDeclaration, error) {
	var j WorkerSetupJournal
	err := root.ReadJSON(backupSetupFile, &j)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return InstallationPaths{}, nil, err
	}
	p, err := declarationPaths(root.File().Name(), j.Declaration)
	return p, j.Declaration, err
}

// Owned files use historical home-relative names or explicitly pinned absolute
// paths. Both forms are opened through a held descriptor with no symlink walk.
func openBackupOwnedParent(root *safefs.Directory, name string) (*safefs.Directory, error) {
	if filepath.IsAbs(name) {
		if !config.WorkerAbsolutePath(name) {
			return nil, fmt.Errorf("invalid owned backup path")
		}
		return safefs.Open(filepath.Dir(name))
	}
	return root.Sub(filepath.Dir(name))
}

func readBackupOwnedFile(root *safefs.Directory, name string, limit int64) ([]byte, error) {
	d, err := openBackupOwnedParent(root, name)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return safefs.ReadFile(d, filepath.Base(name), limit)
}

func openBackupOwnedFile(root *safefs.Directory, name string, flags int, mode uint32) (*os.File, error) {
	d, err := openBackupOwnedParent(root, name)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.Open(filepath.Base(name), flags, mode)
}

func readBackupOwnedJSON(root *safefs.Directory, name string, dest any) error {
	raw, err := readBackupOwnedFile(root, name, safefs.MaxRecordBytes)
	if err != nil {
		return err
	}
	return safefs.DecodeJSON(raw, dest, safefs.MaxRecordBytes)
}

func readInstalledWorkerJSON(root *safefs.Directory, c *WorkerInstallation) error {
	p, declaration, err := installedBackupPaths(root)
	if err != nil {
		return err
	}
	raw, err := readBackupOwnedFile(root, p.OwnedPath(p.Runtime), safefs.MaxRecordBytes)
	if err != nil {
		return err
	}
	var parsed WorkerInstallation
	if err = safefs.DecodeJSON(raw, &parsed, safefs.MaxRecordBytes); err != nil {
		return err
	}
	var expected *InstallationPaths
	if declaration != nil {
		expected = declaration.Paths
	}
	if !reflect.DeepEqual(parsed.Paths, expected) {
		return fmt.Errorf("backup runtime paths differ from retained setup authority")
	}
	*c = parsed
	return nil
}

func loadInstalledWorker(home string) (WorkerInstallation, error) {
	var c WorkerInstallation
	root, err := safefs.Open(home)
	if err != nil {
		return c, err
	}
	defer root.Close()
	if err = readInstalledWorkerJSON(root, &c); err != nil {
		return c, err
	}
	return c, validateWorkerInstallation(c)
}

func installedBackupStatePath(home string) (string, error) {
	p, err := lookupInstalledBackupPaths(home)
	if err != nil {
		return "", err
	}
	return p.State, nil
}

func lookupInstalledBackupPaths(home string) (InstallationPaths, error) {
	root, err := safefs.Open(home)
	if err != nil {
		return InstallationPaths{}, err
	}
	defer root.Close()
	p, _, err := installedBackupPaths(root)
	if err != nil {
		return InstallationPaths{}, err
	}
	return p, nil
}

func openInstalledBackupState(home string, create bool) (*backupState, error) {
	path, err := installedBackupStatePath(home)
	if err != nil {
		return nil, err
	}
	return openBackupState(path, create)
}

func openInstalledBackupStateWithLease(home string, create, resume, installer bool) (*backupState, error) {
	path, err := installedBackupStatePath(home)
	if err != nil {
		return nil, err
	}
	return openBackupStateWithLease(path, create, resume, installer)
}
