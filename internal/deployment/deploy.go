package deployment

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/transport"
	"github.com/dryaf/deploy/internal/ui"
)

func DoRelease(explicitVersion, envName string) {
	if err := release(explicitVersion, envName, CommandRunner{}); err != nil {
		ui.LogFatal("Release failed: %v", err)
	}
}

func release(explicitVersion, envName string, runner ReleaseRunner) error {
	cfg, err := readConfig("deploy.yaml")
	if err != nil {
		return err
	}
	n, err := config.Normalize(cfg, envName)
	if err != nil {
		return err
	}
	// All local configuration and artifact validation precedes Git publication and SSH.
	inputs := n
	inputs.Config.Artifacts.Include = selectedArtifacts(n)
	if err := checkLocalImageArtifacts(inputs); err != nil {
		return err
	}
	if _, err := quadlet.Render(n, ""); err != nil {
		return err
	}
	if !ui.DryRun {
		if err := checkLocalReleaseTools(); err != nil {
			return err
		}
		if n.Mode == config.ModeBinary && cfg.Build.Cmd == "" {
			if _, err := exec.LookPath("go"); err != nil {
				return fmt.Errorf("local prerequisite go missing")
			}
		}
	}
	var projectLock *safefs.Lock
	if !ui.DryRun {
		projectLock, err = safefs.LockProject()
		if err != nil {
			return err
		}
		defer projectLock.Close()
		// Configuration can change while validation or lock acquisition runs.
		cfg, err = readConfig("deploy.yaml")
		if err != nil {
			return err
		}
		n, err = config.Normalize(cfg, envName)
		if err != nil {
			return err
		}
	}
	version, err := resolveAndValidateVersion(explicitVersion)
	if err != nil {
		return err
	}
	commit := "<git-commit>"
	if !ui.DryRun {
		commit = process.GetCmdOutput("git", "rev-parse", "HEAD")
	}
	var p ReleasePlan
	if n.Mode == config.ModeImage {
		p, err = planImageRelease(n, version, commit)
	} else {
		p, err = planBinaryRelease(n, version, commit)
	}
	if err != nil {
		return err
	}
	p.projectLock = projectLock
	p.CheckSource = !ui.DryRun
	if !ui.DryRun {
		p.TagObject = process.GetCmdOutput("git", "rev-parse", "refs/tags/"+version)
	}
	p.Print(os.Stdout)
	if ui.DryRun {
		return nil
	}
	if n.Mode == config.ModeImage {
		return executeImageRelease(p, runner)
	}
	return executeBinaryRelease(p, runner)
}

func executeBinaryRelease(p ReleasePlan, runner ReleaseRunner) error {
	lock := p.projectLock
	if lock == nil {
		var err error
		lock, err = safefs.LockProject()
		if err != nil {
			return err
		}
	}
	defer lock.Close()
	cfg, env, version := p.Config.Config, p.Config.Env, p.Version
	preflight := binaryRemotePreflight + imageResourceChecks(env.Quadlet)
	if env.Database.Driver != "" {
		preflight += databaseReleasePreflight
	}
	if env.Quadlet.HealthURL != "" {
		preflight += "\ncommand -v curl >/dev/null\n"
	}
	if _, err := runner.SSH(env, preflight); err != nil {
		return err
	}
	if err := os.MkdirAll("build", 0755); err != nil {
		return err
	}
	if info, err := os.Lstat("build"); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("build must be a real directory")
	}
	output := filepath.Join("build", cfg.BinaryName)
	if info, err := os.Lstat(output); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("build output must be a regular file: %s", output)
		}
		if err := os.Remove(output); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// 1. Build
	arch := cfg.Build.Arch
	if arch == "" {
		arch = "amd64"
	}
	ui.LogInfo("🔨 Building binary (%s)...", arch)

	buildMeta := getBuildMetadata(version)
	buildMeta.Commit = p.Commit
	buildMeta.Version = p.Version
	buildMeta.Tag = p.Version
	var ldflags string
	if cfg.Build.Ldflags != "" {
		tmpl, err := template.New("ld").Parse(cfg.Build.Ldflags)
		if err != nil {
			return fmt.Errorf("LDFLAGS template: %w", err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, buildMeta); err != nil {
			return err
		}
		ldflags = buf.String()
	} else {
		ldflags = fmt.Sprintf("-s -w -X 'main.buildVersion=%s' -X 'main.buildDate=%s'", buildMeta.Version, buildMeta.Date)
	}

	var cmd *exec.Cmd
	if cfg.Build.Cmd != "" {
		ui.LogInfo("   Using custom build command...")

		// Parse the command string as a template
		tmpl, err := template.New("cmd").Parse(cfg.Build.Cmd)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, buildMeta); err != nil {
			return err
		}
		finalCmd := buf.String()

		ui.LogDebug("   Exec: %s", finalCmd)

		cmd = exec.Command("sh", "-c", finalCmd)
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, fmt.Sprintf("LDFLAGS=%s", ldflags))
	} else {
		srcDir := "."
		if cfg.Build.Dir != "" {
			srcDir = cfg.Build.Dir
		}
		cmd = exec.Command("go", "build", "-ldflags", ldflags, "-o", output, srcDir)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	}

	var buildErr error
	if timed, ok := runner.(interface {
		LocalTimeout(*exec.Cmd, time.Duration, time.Duration) error
	}); ok {
		buildErr = timed.LocalTimeout(cmd, config.Budget(env.Timeouts.Build, "15m"), config.Budget(env.Timeouts.ShutdownGrace, "10s"))
	} else {
		buildErr = runner.Local(cmd)
	}
	if err := buildErr; err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if info, err := os.Lstat(output); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("build did not produce a nonempty regular file at %s", output)
	}

	payload, err := captureRelease(p, output)
	if err != nil {
		return err
	}
	defer payload.Close()
	p.payload = payload
	lock.Close()
	return stageBinaryRelease(p, payload.Container, runner)
}

func DoMaintenanceEnable(envName string) {
	_, env := LoadEnv(envName)

	// Removed the strict check. If the user invokes this command, they want it.
	// Defaults will be applied in generateMaintenance()

	ui.LogInfo("🚧 Setting up Maintenance Page infrastructure on %s...", env.Host)

	if ui.DryRun {
		ui.LogInfo("[DRY] Generate, stage and activate maintenance artifacts for %s", env.Quadlet.ServiceName)
		return
	}
	temporary, err := os.MkdirTemp("", "deploy-maintenance-")
	if err != nil {
		ui.LogFatal("Maintenance staging: %v", err)
	}
	defer os.RemoveAll(temporary)
	maintPath, htmlPath, err := generateMaintenance(env, temporary)
	if err != nil {
		ui.LogFatal("Maintenance generation: %v", err)
	}

	// 2. Sync
	ui.LogInfo("📤 Syncing maintenance artifacts...")
	if err := transport.RunSSH(env, "mkdir -p -- "+transport.ShellQuote(env.Dir+"/maintenance")+" ~/.config/containers/systemd"); err != nil {
		ui.LogFatal("Maintenance directory: %v", err)
	}
	transport.RunRsync(env, []string{htmlPath}, fmt.Sprintf("%s@%s:%s/maintenance/index.html", env.User, env.Host, env.Dir))
	transport.RunRsync(env, []string{maintPath}, remoteDestination(env, ".config/containers/systemd/"))

	// 3. Activate
	serviceName := env.Quadlet.ServiceName + "-maint"
	ui.LogInfo("🔄 Activating maintenance container (%s)...", serviceName)

	script := strings.Join([]string{
		"systemctl --user daemon-reload",
		"mkdir -p ~/.config/systemd/user/default.target.wants",
		// Manually link to enable persistence (workaround for 'failed to enable unit: generated' error)
		fmt.Sprintf("ln -sf /run/user/$(id -u)/systemd/generator/%s.service ~/.config/systemd/user/default.target.wants/%s.service", serviceName, serviceName),
		// Just Start (enable is covered by the link above)
		fmt.Sprintf("systemctl --user start %s.service", serviceName),
		// Verify
		fmt.Sprintf("sleep 2 && systemctl --user is-active %s.service", serviceName),
	}, " && ")

	if err := transport.RunSSH(env, script); err != nil {
		ui.LogFatal("Failed to start maintenance container: %v", err)
	}

	ui.LogSuccess("✅ Maintenance page is UP (Priority 1).")
	ui.LogInfo("   It will be served automatically whenever '%s' (Priority 100) is stopped.", env.Quadlet.ServiceName)
}

func DoMaintenanceDisable(envName string) {
	_, env := LoadEnv(envName)
	serviceName := env.Quadlet.ServiceName + "-maint"

	ui.LogInfo("🚧 Disabling Maintenance Page on %s...", env.Host)

	script := strings.Join([]string{
		// 1. Stop the service (ignore error if not running)
		fmt.Sprintf("systemctl --user stop %s.service || true", serviceName),
		// 2. Remove the manual persistent link
		fmt.Sprintf("rm -f ~/.config/systemd/user/default.target.wants/%s.service", serviceName),
		// 3. Remove the Quadlet definition so it's not regenerated
		fmt.Sprintf("rm -f ~/.config/containers/systemd/%s.container", serviceName),
		// 4. Reload systemd to reflect removal
		"systemctl --user daemon-reload",
	}, " && ")

	if err := transport.RunSSH(env, script); err != nil {
		ui.LogFatal("Failed to disable maintenance page: %v", err)
	}

	// Optional: We leave the HTML file in place as it's harmless and useful for future enables.
	ui.LogSuccess("✅ Maintenance page disabled and removed.")
}

// resolveAndValidateVersion handles the logic for strict versioning and "lazy" tagging.
var releaseVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+-]*$`)

func validateReleaseVersion(version string) error {
	if !releaseVersionPattern.MatchString(version) {
		return fmt.Errorf("release tag must start with a letter or digit and use only letters, digits, dot, underscore, slash, plus or hyphen")
	}
	return nil
}

func resolveAndValidateVersion(explicitVersion string) (string, error) {
	if explicitVersion != "" {
		if err := validateReleaseVersion(explicitVersion); err != nil {
			return "", err
		}
	}
	if ui.DryRun {
		if explicitVersion == "" {
			return "<configuration-version>", nil
		}
		return explicitVersion, nil
	}
	out, err := process.GitOutput("status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("git status: %w", err)
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return "", fmt.Errorf("Git working directory is dirty; commit or stash changes before releasing")
	}
	hasRemote := process.GitRun("remote", "get-url", "origin") == nil
	if explicitVersion != "" {
		// Accept tags only, never revision expressions or command-line options.
		if err := process.GitRun("check-ref-format", "refs/tags/"+explicitVersion); err != nil {
			return "", fmt.Errorf("invalid Git tag")
		}
		head, err := process.GitOutput("rev-parse", "--verify", "HEAD")
		if err != nil {
			return "", err
		}
		tag, err := process.GitOutput("rev-parse", "--verify", "refs/tags/"+explicitVersion+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("tag %q not found: %w", explicitVersion, err)
		}
		if !bytes.Equal(head, tag) {
			return "", fmt.Errorf("HEAD is not at tag %s; check out the tag first", explicitVersion)
		}
		if hasRemote {
			if err := ensureTagPushed(explicitVersion); err != nil {
				return "", err
			}
		}
		return explicitVersion, nil
	}
	if tag, err := process.GitOutput("describe", "--tags", "--exact-match", "HEAD"); err == nil {
		version := strings.TrimSpace(string(tag))
		if err := validateReleaseVersion(version); err != nil {
			return "", err
		}
		if hasRemote {
			if err := ensureTagPushed(version); err != nil {
				return "", err
			}
		}
		return version, nil
	}
	// A pager cannot interact with the terminal from runBounded's process group.
	if err := process.RunCommandRaw("git", "--no-pager", "for-each-ref", "--sort=-v:refname", "--count=4", "--format=%(refname:strip=2)", "refs/tags/"); err != nil {
		return "", err
	}
	version := ui.Prompt("Enter new semantic version (e.g. v1.0.1)")
	if err := validateReleaseVersion(version); err != nil {
		return "", err
	}
	if version == "" || strings.HasPrefix(version, "-") {
		return "", fmt.Errorf("valid version tag required")
	}
	if err := process.GitRun("check-ref-format", "refs/tags/"+version); err != nil {
		return "", fmt.Errorf("invalid Git tag")
	}
	if err := process.RunCommandRaw("git", "tag", "-a", version, "-m", "Release "+version); err != nil {
		return "", err
	}
	if hasRemote {
		if err := process.RunCommandRaw("git", "push", "origin", "refs/tags/"+version); err != nil {
			return "", err
		}
		if exists, err := verifyRemoteTag(version); err != nil || !exists {
			return "", fmt.Errorf("verify published tag %s: %v", version, err)
		}
	}
	return version, nil
}

func ensureTagPushed(version string) error {
	exists, err := verifyRemoteTag(version)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if !ui.Confirm(fmt.Sprintf("Push tag %s to origin?", version)) {
		return fmt.Errorf("deployment requires synced Git tags")
	}
	if err := process.RunCommandRaw("git", "push", "origin", "refs/tags/"+version); err != nil {
		return err
	}
	exists, err = verifyRemoteTag(version)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("remote tag %s missing after push", version)
	}
	return nil
}

func verifyRemoteTag(version string) (bool, error) {
	ref := "refs/tags/" + version
	if err := process.GitRun("check-ref-format", ref); err != nil {
		return false, fmt.Errorf("invalid Git tag")
	}
	local, err := process.GitOutput("rev-parse", "--verify", ref)
	if err != nil {
		return false, fmt.Errorf("local tag %s: %w", version, err)
	}
	peeled, err := process.GitOutput("rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return false, fmt.Errorf("tag %s must identify a commit: %w", version, err)
	}
	out, err := process.GitOutput("ls-remote", "--tags", "origin", ref, ref+"^{}")
	if err != nil {
		return false, fmt.Errorf("read remote tag %s: %w", version, err)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[1] != ref && fields[1] != ref+"^{}") || refs[fields[1]] != "" {
			return false, fmt.Errorf("malformed remote tag response")
		}
		refs[fields[1]] = fields[0]
	}
	if len(refs) == 0 {
		return false, nil
	}
	if refs[ref] != strings.TrimSpace(string(local)) {
		return false, fmt.Errorf("tag %s differs: local %s; remote %s", version, strings.TrimSpace(string(local)), refs[ref])
	}
	if target := refs[ref+"^{}"]; target != "" && target != strings.TrimSpace(string(peeled)) {
		return false, fmt.Errorf("peeled remote tag %s differs from local commit", version)
	}
	return true, nil
}

func getBuildMetadata(explicitVersion string) config.BuildMetadata {
	get := func(args ...string) string {
		if ui.DryRun {
			return "dry"
		}
		out, _ := process.Output(exec.Command(args[0], args[1:]...), 2*time.Minute)
		return strings.TrimSpace(string(out))
	}

	// If explicit version passed, use it. Otherwise fall back to git describe
	v := explicitVersion
	if v == "" {
		v = get("git", "describe", "--tags", "--always", "--dirty")
	}

	// Calculate MainVersion (e.g. v1.2.3 -> v1.2)
	mainVer := v
	cleanVer := strings.TrimPrefix(v, "v") // handle v1.2.3 -> 1.2.3
	parts := strings.Split(cleanVer, ".")
	if len(parts) >= 2 {
		// Re-add 'v' if it was present, purely heuristic
		prefix := ""
		if strings.HasPrefix(v, "v") {
			prefix = "v"
		}
		mainVer = prefix + parts[0] + "." + parts[1]
	}

	return config.BuildMetadata{
		Version:     v,
		Date:        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Tag:         v,
		Commit:      get("git", "rev-parse", "HEAD"),
		MainVersion: mainVer,
		GoVersion:   runtime.Version(),
	}
}

func generateMaintenance(env config.Environment, outDir string) (string, string, error) {
	// 1. Apply Defaults if config is missing (Enabled check removed in CLI)
	if env.Maintenance.Title == "" {
		env.Maintenance.Title = "System Maintenance"
	}
	if env.Maintenance.Text == "" {
		env.Maintenance.Text = "The system is currently undergoing maintenance. We will be back shortly."
	}

	// 2. Generate HTML
	var htmlBuf bytes.Buffer
	t1, err := template.New("h").Parse(quadlet.MaintenanceHtmlTmpl)
	if err != nil {
		return "", "", err
	}
	if err = t1.Execute(&htmlBuf, env.Maintenance); err != nil {
		return "", "", err
	}

	htmlPath := filepath.Join(outDir, "maintenance.html")
	if !ui.DryRun {
		if err = os.WriteFile(htmlPath, htmlBuf.Bytes(), 0644); err != nil {
			return "", "", err
		}
	}

	// 3. Generate Container
	resolver := "myresolver" // Default convention

	// Determine the Rule (Priority: explicit rule > domain > host)
	rule := env.Quadlet.Router.Rule
	if rule == "" {
		if env.Quadlet.Router.Domain != "" {
			rule = fmt.Sprintf("Host(`%s`)", env.Quadlet.Router.Domain)
		} else if env.Quadlet.Router.Host != "" {
			rule = fmt.Sprintf("Host(`%s`)", env.Quadlet.Router.Host)
		} else {
			// Fallback if both missing
			rule = "Host(`unknown-host`)"
		}
	}

	data := quadlet.MaintenanceTemplateData{
		ServiceName: env.Quadlet.ServiceName,
		Rule:        rule,
		Network:     env.Quadlet.Network,
		TargetDir:   env.Dir,
		Resolver:    resolver,
	}
	var cBuf bytes.Buffer
	t2, err := template.New("mc").Parse(quadlet.MaintenanceContainerTmpl)
	if err != nil {
		return "", "", err
	}
	if err = t2.Execute(&cBuf, data); err != nil {
		return "", "", err
	}

	containerPath := filepath.Join(outDir, env.Quadlet.ServiceName+"-maint.container")
	if !ui.DryRun {
		if err = os.WriteFile(containerPath, cBuf.Bytes(), 0644); err != nil {
			return "", "", err
		}
	}

	return containerPath, htmlPath, nil
}
