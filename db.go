package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed db_transaction.sh
var databaseTransactionBody string

var databaseTransactionScript = transactionCommonScript + "\n" + databaseTransactionBody

const databaseReleasePreflight = `
command -v sqlite3 >/dev/null || { echo 'Configured database requires sqlite3' >&2; exit 1; }
sqlite3 :memory: '.help dump' | grep -q -- '--preserve-rowids' || { echo 'SQLite CLI with .dump --preserve-rowids is required' >&2; exit 1; }
`

type databaseRequest struct {
	Schema      int            `json:"schema"`
	ReleaseID   string         `json:"release_id"`
	Service     string         `json:"service"`
	Target      string         `json:"target"`
	Mode        DeploymentMode `json:"deployment_mode"`
	Database    string         `json:"database"`
	UID         int            `json:"container_uid"`
	GID         int            `json:"container_gid"`
	Environment string         `json:"environment"`
}

func configureReleaseDatabase(req *imageReleaseRequest, env Environment) {
	req.Environment = env.Name
	req.UID, req.GID = env.Quadlet.ContainerUID, env.Quadlet.ContainerGID
	if env.Database.Driver == "sqlite" {
		req.Database = path.Join(env.Dir, env.Database.Source)
		req.DatabaseRollback = env.Database.Rollback
	}
}

func databasePaths(env Environment) (local, remote string, err error) {
	if env.Database.Driver != "sqlite" {
		return "", "", fmt.Errorf("only sqlite databases are supported")
	}
	if err := validateArtifactPath(env.Database.Source); err != nil {
		return "", "", fmt.Errorf("database source: %w", err)
	}
	return filepath.Clean(env.Database.Source), path.Join(env.Dir, env.Database.Source), nil
}

func doDBPull(envName string) {
	_, env := loadEnv(envName)
	local, _, err := databasePaths(env)
	if err != nil {
		logFatal("Database pull: %v", err)
	}
	if dryRun {
		logInfo("[DRY] Download SQLite snapshot to %s; retain a timestamped backup in its sibling backup directory", local)
		return
	}
	if !confirm(fmt.Sprintf("Download to %s and back up the existing database? Stop your local application first.", local)) {
		return
	}
	if err := pullDatabase(env); err != nil {
		logFatal("Database pull: %v", err)
	}
	logSuccess("Synced to %s", local)
}

func doDBPush(envName string) {
	cfg, env := loadEnv(envName)
	if _, _, err := databasePaths(env); err != nil {
		logFatal("Database push: %v", err)
	}
	if dryRun {
		logInfo("[DRY] Stage SQLite snapshot; lock service and target; require stopped service and successful backup before replacement")
		return
	}
	if !confirm("Overwrite the remote database after taking a backup?") {
		return
	}
	if err := pushDatabase(env, cfg.Deployment.Mode, commandRunner{}); err != nil {
		logFatal("Database push: %v", err)
	}
	logSuccess("Database pushed; service remains stopped.")
}

func sqliteSnapshot(source, destination string) error {
	s, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !s.Mode().IsRegular() {
		return fmt.Errorf("database source must be a regular file")
	}
	out, err := exec.Command("sqlite3", "-readonly", source, ".backup "+strconv.Quote(destination)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("SQLite snapshot: %w: %s", err, out)
	}
	if err := os.Chmod(destination, 0600); err != nil {
		return err
	}
	if err := validateSQLiteSnapshot(destination); err != nil {
		return err
	}
	return syncFile(destination)
}

func validateSQLiteSnapshot(file string) error {
	out, err := exec.Command("sqlite3", "-readonly", file, "PRAGMA quick_check;").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		return fmt.Errorf("SQLite snapshot failed integrity validation: %s", out)
	}
	return nil
}

func filenameLabel(value string) string {
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, value)
	if value == "" {
		return "unknown"
	}
	return value
}

func backupLocalDatabase(database, environment, operation string) (string, error) {
	dir := filepath.Join(filepath.Dir(database), "backup")
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("backup must be a real directory: %s", dir)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	id, err := newReleaseID("backup")
	if err != nil {
		return "", err
	}
	name := strings.TrimSuffix(filepath.Base(database), filepath.Ext(database)) + "." + filenameLabel(environment) + "." + filenameLabel(operation) + "." + strings.TrimPrefix(id, "backup-") + ".db"
	backup := filepath.Join(dir, name)
	temp, err := os.CreateTemp(dir, ".snapshot-")
	if err != nil {
		return "", err
	}
	temp.Close()
	defer os.Remove(temp.Name())
	if err := sqliteSnapshot(database, temp.Name()); err != nil {
		return "", err
	}
	if err := os.Rename(temp.Name(), backup); err != nil {
		return "", err
	}
	if err := syncFile(dir); err != nil {
		return "", err
	}
	if err := syncFile(filepath.Dir(dir)); err != nil {
		return "", err
	}
	logInfo("Database backup: %s", backup)
	return backup, nil
}

func syncFile(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func pullDatabase(env Environment) error {
	local, remote, err := databasePaths(env)
	if err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return fmt.Errorf("local sqlite3 is required")
	}
	if err := prepareSSHControlDir(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(local), 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(local), ".deploy-db-pull-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	download := filepath.Join(dir, "download.db")
	f, err := os.OpenFile(download, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	// Fixed relative output avoids interpolating a path into SQLite's dot-command grammar.
	sqlite := "sqlite3"
	remoteTest := "test"
	if env.Quadlet.ContainerUID > 0 {
		sqlite = "podman unshare sqlite3"
		remoteTest = "podman unshare test"
	}
	script := "set -euo pipefail\numask 077\nsource=" + shellQuote(remote) + "\n" + remoteTest + " -f \"$source\" || { echo 'Remote database does not exist' >&2; exit 1; }\n" + `temp=$(mktemp -d)
trap 'rm -rf -- "$temp"' EXIT
cd -- "$temp"
` + sqlite + " -readonly \"$source\" '.backup backup.db'\ncat -- backup.db\n"
	cmd := exec.Command("ssh", append(getSSHBaseArgs(env), "bash -c "+shellQuote(script))...)
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	syncErr := f.Sync()
	closeErr := f.Close()
	if runErr != nil {
		return fmt.Errorf("download snapshot: %w", runErr)
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := validateSQLiteSnapshot(download); err != nil {
		return err
	}
	if _, err := os.Lstat(local); err == nil {
		backup, err := backupLocalDatabase(local, "local", "before-pull-from-"+filenameLabel(env.Name))
		if err != nil {
			return err
		}
		// Keep the legacy latest-backup file, while retaining every unique snapshot.
		if err := sqliteSnapshot(backup, filepath.Join(dir, "latest.db")); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(dir, "latest.db"), local+".bak"); err != nil {
			return err
		}
		if err := syncFile(filepath.Dir(local)); err != nil {
			return err
		}
		// SQLite's backup API publishes the replacement in a database transaction.
		// Its journal/WAL handles process crashes; never detach sidecars by hand.
		out, err := exec.Command("sqlite3", "-cmd", ".timeout 5000", local, ".restore "+strconv.Quote(download)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("restore downloaded snapshot: %w: %s", err, out)
		}
		if err := syncFile(local); err != nil {
			return err
		}
		return syncFile(filepath.Dir(local))
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(local + suffix); !os.IsNotExist(err) {
			return fmt.Errorf("orphan database sidecar requires recovery before pull: %s", local+suffix)
		}
	}
	if err := os.Rename(download, local); err != nil {
		return err
	}
	return syncFile(filepath.Dir(local))
}

func pushDatabase(env Environment, mode DeploymentMode, runner ReleaseRunner) error {
	local, remote, err := databasePaths(env)
	if err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	temp, err := os.MkdirTemp("", "deploy-db-push-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	snapshot := filepath.Join(temp, "upload.db")
	if err := sqliteSnapshot(local, snapshot); err != nil {
		return err
	}
	id, err := newReleaseID("database")
	if err != nil {
		return err
	}
	if mode == "" {
		mode = ModeBinary
	}
	req := databaseRequest{Schema: 1, ReleaseID: id, Service: env.Quadlet.ServiceName, Target: env.Dir, Mode: mode, Database: remote, UID: env.Quadlet.ContainerUID, GID: env.Quadlet.ContainerGID, Environment: env.Name}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{"request.json": body, "transaction.sh": []byte(databaseTransactionScript)} {
		if err := os.WriteFile(filepath.Join(temp, name), data, 0600); err != nil {
			return err
		}
	}
	stage := path.Join(env.Dir, ".deploy/db/staging", id)
	preflight := binaryRemotePreflight + "\ncommand -v sqlite3 >/dev/null\n" + safeRemoteMkdir(stage)
	if _, err := runner.SSH(env, preflight); err != nil {
		return err
	}
	if err := runner.Sync(env, []string{snapshot, filepath.Join(temp, "request.json"), filepath.Join(temp, "transaction.sh")}, remoteDestination(env, stage+"/"), "--chmod=F600"); err != nil {
		return fmt.Errorf("stage database: %w", err)
	}
	out, err := runner.SSH(env, "bash "+shellQuote(stage+"/transaction.sh")+" release "+shellQuote(stage))
	if out != "" {
		fmt.Print(out)
	}
	return err
}
