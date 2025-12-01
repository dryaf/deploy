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

	logWarn("🔥 PUSHING DB to %s. Service will STOP.", envName)
	if !confirm("Sure?") {
		return
	}

	// 1. Stop Service
	logInfo("🛑 Stopping service...")
	if err := runSSH(env, fmt.Sprintf("systemctl --user stop %s.service", env.Quadlet.ServiceName)); err != nil {
		logFatal("Failed to stop service: %v", err)
	}

	err := func() error {
		// 2. Permission Fix (if needed)
		if env.Quadlet.ContainerUID > 0 {
			logInfo("🔧 Reclaiming file permissions...")
			runSSH(env, fmt.Sprintf("podman unshare chown $(id -u):$(id -g) %s %s-wal %s-shm || true", remote, remote, remote))
		}

		// 3. Backup Remote
		logInfo("📦 Creating remote backup...")
		if err := runSSH(env, fmt.Sprintf("cp %s %s.bak || true", remote, remote)); err != nil {
			return fmt.Errorf("remote backup failed: %v", err)
		}
		runSSH(env, fmt.Sprintf("rm -f %s-wal %s-shm", remote, remote))

		// 4. Upload
		logInfo("📤 Uploading...")
		if err := runRsyncSafe(env, []string{local}, fmt.Sprintf("%s@%s:%s", env.User, env.Host, remote)); err != nil {
			logError("Rsync failed: %v", err)
			logInfo("Restoring from backup...")
			runSSH(env, fmt.Sprintf("mv %s.bak %s", remote, remote))
			return err
		}
		return nil
	}()

	// 5. Restore Permissions
	if env.Quadlet.ContainerUID > 0 {
		logInfo("🔧 Restoring container permissions...")
		runSSH(env, fmt.Sprintf("podman unshare chown %d:%d %s %s.bak", env.Quadlet.ContainerUID, env.Quadlet.ContainerGID, remote, remote))
	}

	// 6. Start Service
	logInfo("▶️ Starting service...")
	runSSH(env, fmt.Sprintf("systemctl --user start %s.service", env.Quadlet.ServiceName))

	if err != nil {
		logFatal("Push failed: %v", err)
	}
	logSuccess("Pushed successfully.")
}
