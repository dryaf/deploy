package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func doDBPull(envName string) {
	_, env := loadEnv(envName)
	if env.Database.Driver != "sqlite" {
		logFatal("Only sqlite supported")
	}

	local := filepath.Clean(env.Database.Source)
	remote := fmt.Sprintf("%s/%s", strings.TrimRight(env.Dir, "/"), env.Database.Source)

	logInfo("📥 Pulling DB from %s...", env.Host)

	// Backup Local DB if it exists
	if _, err := os.Stat(local); err == nil {
		if !confirm(fmt.Sprintf("Local file %s exists. Backup and overwrite?", local)) {
			return
		}
		backup := local + ".bak"
		logInfo("📦 Backing up local DB to %s...", backup)
		if err := copyFile(local, backup); err != nil {
			logFatal("Failed to backup local file: %v", err)
		}
	} else {
		if !confirm(fmt.Sprintf("Download to %s?", local)) {
			return
		}
	}

	if !dryRun {
		os.MkdirAll(filepath.Dir(local), 0755)
	}

	f, err := os.Create(local)
	if err != nil {
		logFatal("Failed to create local file: %v", err)
	}
	defer f.Close()

	// Robust Backup Strategy
	remoteScript := fmt.Sprintf(`
		set -e
		TEMP_DIR=$(mktemp -d)
		trap "rm -rf $TEMP_DIR" EXIT
		if ! command -v sqlite3 &> /dev/null; then
            echo "sqlite3 not found on remote" >&2
            exit 1
        fi
		sqlite3 '%s' ".backup '$TEMP_DIR/backup.db'"
		cat "$TEMP_DIR/backup.db"
	`, remote)

	sshArgs := getSSHBaseArgs(env)
	sshArgs = append(sshArgs, remoteScript)

	cmd := exec.Command("ssh", sshArgs...)
	cmd.Stdout = f
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		f.Close()
		os.Remove(local)
		logFatal("Pull failed: %v", err)
	}
	logSuccess("Synced to %s", local)
}

func doDBPush(envName string) {
	_, env := loadEnv(envName)
	local := filepath.Clean(env.Database.Source)
	remote := fmt.Sprintf("%s/%s", strings.TrimRight(env.Dir, "/"), env.Database.Source)

	// 1. Safety Check: Is service running?
	// In dry-run, we skip this check because runSSH returns nil (success) which would trigger false positive.
	if !dryRun {
		// systemctl is-active returns 0 (success) if running, which means err == nil
		err := runSSH(env, fmt.Sprintf("systemctl --user is-active -q %s.service", env.Quadlet.ServiceName))
		if err == nil {
			logFatal("⛔ Service '%s' is RUNNING on %s.\n   You must manually stop it before pushing a database to prevent corruption.\n   Run: deploy stop %s", env.Quadlet.ServiceName, env.Host, envName)
		}
	}

	logWarn("🔥 OVERWRITING REMOTE DB on %s.", envName)
	if !confirm("Are you sure?") {
		return
	}

	// 2. Permission Fix (if needed) - Pre-transfer
	if env.Quadlet.ContainerUID > 0 {
		logInfo("🔧 Reclaiming file permissions...")
		runSSH(env, fmt.Sprintf("podman unshare chown $(id -u):$(id -g) %s %s-wal %s-shm || true", remote, remote, remote))
	}

	// 3. Backup Remote
	logInfo("📦 Creating remote backup...")
	if err := runSSH(env, fmt.Sprintf("cp %s %s.bak || true", remote, remote)); err != nil {
		logFatal("Remote backup failed: %v", err)
	}
	// Clean up WAL/SHM to ensure clean state
	runSSH(env, fmt.Sprintf("rm -f %s-wal %s-shm", remote, remote))

	// 4. Upload
	logInfo("📤 Uploading...")
	if err := runRsyncSafe(env, []string{local}, fmt.Sprintf("%s@%s:%s", env.User, env.Host, remote)); err != nil {
		logError("Rsync failed: %v", err)
		logInfo("Restoring from backup...")
		runSSH(env, fmt.Sprintf("mv %s.bak %s", remote, remote))
		logFatal("Upload failed and backup restored.")
	}

	// 5. Restore Permissions
	if env.Quadlet.ContainerUID > 0 {
		logInfo("🔧 Restoring container permissions...")
		runSSH(env, fmt.Sprintf("podman unshare chown %d:%d %s %s.bak", env.Quadlet.ContainerUID, env.Quadlet.ContainerGID, remote, remote))
	}

	logSuccess("Database pushed successfully.")
	logInfo("ℹ️  Service remains STOPPED. Run 'deploy start %s' or 'deploy release %s' when ready.", envName, envName)
}
