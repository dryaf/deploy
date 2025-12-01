package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

func doRun(envName string) {
	cfg, env := loadEnv(envName)

	if _, err := exec.LookPath("rsync"); err != nil {
		logFatal("Local rsync missing")
	}

	// Pre-flight checks
	logInfo("🔍 Verifying remote environment on %s...", env.Host)
	if err := runSSH(env, "command -v rsync >/dev/null && command -v podman >/dev/null"); err != nil {
		logFatal("Remote check failed: 'rsync' and 'podman' are required on the host.")
	}

	logInfo("🚀 Deploying %s to %s...", cfg.AppName, envName)

	if !dryRun {
		os.MkdirAll("build", 0755)
	}

	// 1. Build
	arch := cfg.Build.Arch
	if arch == "" {
		arch = "amd64"
	}
	logInfo("🔨 Building binary (%s)...", arch)

	buildMeta := getBuildMetadata()
	var ldflags string
	if cfg.Build.Ldflags != "" {
		tmpl, err := template.New("ld").Parse(cfg.Build.Ldflags)
		if err != nil {
			logFatal("LDFLAGS template error: %v", err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, buildMeta); err != nil {
			logFatal("LDFLAGS exec: %v", err)
		}
		ldflags = buf.String()
	} else {
		ldflags = fmt.Sprintf("-s -w -X 'main.buildVersion=%s' -X 'main.buildDate=%s'", buildMeta.Version, buildMeta.Date)
	}

	var cmd *exec.Cmd
	if cfg.Build.Cmd != "" {
		logInfo("   Using custom build command...")
		cmd = exec.Command("sh", "-c", cfg.Build.Cmd)
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, fmt.Sprintf("LDFLAGS=%s", ldflags))
	} else {
		srcDir := "."
		if cfg.Build.Dir != "" {
			srcDir = cfg.Build.Dir
		}
		output := fmt.Sprintf("build/%s", cfg.BinaryName)
		cmd = exec.Command("go", "build", "-ldflags", ldflags, "-o", output, srcDir)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	}

	if err := runCommand("Build", cmd); err != nil {
		logFatal("Build failed: %v", err)
	}

	// 2. Generate Configuration
	logInfo("📄 Generating configuration...")
	env.Quadlet.Labels = generateTraefikLabels(env.Quadlet.ServiceName, env.Quadlet.Router, env.Traefik.CertResolver)
	containerPath := generateQuadlet(env, "build")

	// 3. Sync
	logInfo("📤 Syncing...")
	runSSH(env, fmt.Sprintf("mkdir -p %s/data %s/migrations ~/.config/containers/systemd", env.Dir, env.Dir))

	binPath := fmt.Sprintf("%s/%s", env.Dir, cfg.BinaryName)
	runSSH(env, fmt.Sprintf("[ -f %s ] && cp %s %s.bak || true", binPath, binPath, binPath))

	artifacts := []string{}
	artifacts = append(artifacts, "build/"+cfg.BinaryName)
	if len(cfg.Artifacts.Include) > 0 {
		artifacts = append(artifacts, cfg.Artifacts.Include...)
	} else {
		artifacts = append(artifacts, "Dockerfile.vps", "migrations/", "files/")
	}

	runRsync(env, artifacts, fmt.Sprintf("%s@%s:%s/", env.User, env.Host, env.Dir), "--delete")

	if env.SyncEnvFile != "" {
		runRsync(env, []string{env.SyncEnvFile}, fmt.Sprintf("%s@%s:%s/.env", env.User, env.Host, env.Dir))
	}
	runRsync(env, []string{containerPath}, fmt.Sprintf("%s@%s:~/.config/containers/systemd/", env.User, env.Host))

	// 4. Activate
	logInfo("🔄 Activating...")
	permCmd := "true"
	if env.Quadlet.ContainerUID > 0 && len(env.Quadlet.ChownVolumes) > 0 {
		var paths []string
		for _, p := range env.Quadlet.ChownVolumes {
			if strings.HasPrefix(p, "./") {
				p = fmt.Sprintf("%s/%s", strings.TrimRight(env.Dir, "/"), strings.TrimPrefix(p, "./"))
			}
			paths = append(paths, p)
		}
		if len(paths) > 0 {
			permCmd = fmt.Sprintf("podman unshare chown -R %d:%d %s", env.Quadlet.ContainerUID, env.Quadlet.ContainerGID, strings.Join(paths, " "))
		}
	}

	dockerfile := env.Quadlet.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile.vps"
	}

	script := strings.Join([]string{
		fmt.Sprintf("cd %s", env.Dir),
		fmt.Sprintf("podman build -f %s -t %s .", dockerfile, env.Quadlet.Image),
		permCmd,
		"systemctl --user daemon-reload",
		"mkdir -p ~/.config/systemd/user/default.target.wants",
		fmt.Sprintf("ln -sf /run/user/$(id -u)/systemd/generator/%s.service ~/.config/systemd/user/default.target.wants/%s.service", env.Quadlet.ServiceName, env.Quadlet.ServiceName),
		"systemctl --user daemon-reload",
		fmt.Sprintf("systemctl --user restart %s.service", env.Quadlet.ServiceName),
		fmt.Sprintf("sleep 2 && systemctl --user is-active %s.service", env.Quadlet.ServiceName),
	}, " && ")

	if err := runSSH(env, script); err != nil {
		logError("Activation failed: %v", err)
		rollback(env, binPath, dockerfile)
		logFatal("Deployment failed but successfully rolled back.")
	}

	// 5. App Health Check
	if env.Quadlet.HealthURL != "" {
		logInfo("🩺 Performing Application Health Check (%s)...", env.Quadlet.HealthURL)
		checkScript := fmt.Sprintf(`
			for i in {1..15}; do
				if curl -s -f "%s" > /dev/null; then
					echo "OK"
					exit 0
				fi
				sleep 2
			done
			echo "Health check timed out"
			exit 1
		`, env.Quadlet.HealthURL)

		if err := runSSH(env, checkScript); err != nil {
			logError("Health Check failed!")
			rollback(env, binPath, dockerfile)
			logFatal("Deployment failed (Unhealthy) but successfully rolled back.")
		}
	}

	logSuccess("✅ Deployed successfully.")
}

func rollback(env Environment, binPath, dockerfile string) {
	logWarn("🔍 Diagnosing with remote logs (last 50 lines)...")
	runSSHStream(env, fmt.Sprintf("journalctl --user -u %s.service -n 50 --no-pager", env.Quadlet.ServiceName))

	logWarn("🚨 INITIATING AUTOMATIC ROLLBACK...")
	rbScript := strings.Join([]string{
		fmt.Sprintf("cd %s", env.Dir),
		fmt.Sprintf("[ -f %s.bak ] && mv %s.bak %s", binPath, binPath, binPath),
		fmt.Sprintf("podman build -f %s -t %s .", dockerfile, env.Quadlet.Image),
		fmt.Sprintf("systemctl --user restart %s.service", env.Quadlet.ServiceName),
	}, " && ")
	if rbErr := runSSH(env, rbScript); rbErr != nil {
		logFatal("CRITICAL: Rollback failed! Error: %v", rbErr)
	}
}

func getBuildMetadata() BuildMetadata {
	get := func(args ...string) string {
		if dryRun {
			return "dry"
		}
		out, _ := exec.Command(args[0], args[1:]...).Output()
		return strings.TrimSpace(string(out))
	}
	return BuildMetadata{
		Version: get("git", "rev-parse", "--short", "HEAD"),
		Date:    time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Tag:     get("git", "describe", "--tags", "--abbrev=0"),
		Commit:  get("git", "rev-parse", "HEAD"),
	}
}

func generateQuadlet(env Environment, outDir string) string {
	var absVolumes []string
	for _, vol := range env.Quadlet.Volumes {
		parts := strings.Split(vol, ":")
		if len(parts) > 0 && strings.HasPrefix(parts[0], "./") {
			rel := strings.TrimPrefix(parts[0], "./")
			abs := strings.TrimRight(env.Dir, "/") + "/" + rel
			parts[0] = abs
			absVolumes = append(absVolumes, strings.Join(parts, ":"))
		} else {
			absVolumes = append(absVolumes, vol)
		}
	}
	data := TemplateData{Quadlet: env.Quadlet, TargetDir: env.Dir}
	data.Quadlet.Volumes = absVolumes

	var buf bytes.Buffer
	t, _ := template.New("q").Parse(quadletTemplate)
	t.Execute(&buf, data)
	path := filepath.Join(outDir, env.Quadlet.ServiceName+".container")
	if !dryRun {
		os.WriteFile(path, buf.Bytes(), 0644)
	}
	return path
}
