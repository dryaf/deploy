package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"text/template"
	"time"
)

func doRelease(explicitVersion, envName string) {
	if err := release(explicitVersion, envName, commandRunner{}); err != nil {
		logFatal("Release failed: %v", err)
	}
}

func release(explicitVersion, envName string, runner ReleaseRunner) error {
	cfg, err := readConfig("deploy.yaml")
	if err != nil {
		return err
	}
	n, err := normalizeAndValidateConfig(cfg, envName)
	if err != nil {
		return err
	}
	// All local configuration and artifact validation precedes Git publication and SSH.
	inputs := n
	inputs.Config.Artifacts.Include = selectedArtifacts(n)
	if err := checkLocalImageArtifacts(inputs); err != nil {
		return err
	}
	if _, err := renderQuadlet(n, ""); err != nil {
		return err
	}
	if !dryRun {
		if err := checkLocalReleaseTools(); err != nil {
			return err
		}
		if n.Mode == ModeBinary && cfg.Build.Cmd == "" {
			if _, err := exec.LookPath("go"); err != nil {
				return fmt.Errorf("local prerequisite go missing")
			}
		}
	}
	var projectLock *localLock
	if !dryRun {
		projectLock, err = lockProject()
		if err != nil {
			return err
		}
		defer projectLock.Close()
		// Configuration can change while validation or lock acquisition runs.
		cfg, err = readConfig("deploy.yaml")
		if err != nil {
			return err
		}
		n, err = normalizeAndValidateConfig(cfg, envName)
		if err != nil {
			return err
		}
	}
	version, err := resolveAndValidateVersion(explicitVersion)
	if err != nil {
		return err
	}
	commit := "<git-commit>"
	if !dryRun {
		commit = getCmdOutput("git", "rev-parse", "HEAD")
	}
	var p ReleasePlan
	if n.Mode == ModeImage {
		p, err = planImageRelease(n, version, commit)
	} else {
		p, err = planBinaryRelease(n, version, commit)
	}
	if err != nil {
		return err
	}
	p.projectLock = projectLock
	p.CheckSource = !dryRun
	if !dryRun {
		p.TagObject = getCmdOutput("git", "rev-parse", "refs/tags/"+version)
	}
	p.Print(os.Stdout)
	if dryRun {
		return nil
	}
	if n.Mode == ModeImage {
		return executeImageRelease(p, runner)
	}
	return executeBinaryRelease(p, runner)
}

func executeBinaryRelease(p ReleasePlan, runner ReleaseRunner) error {
	lock := p.projectLock
	if lock == nil {
		var err error
		lock, err = lockProject()
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
	logInfo("🔨 Building binary (%s)...", arch)

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
		logInfo("   Using custom build command...")

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

		logDebug("   Exec: %s", finalCmd)

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
		buildErr = timed.LocalTimeout(cmd, budget(env.Timeouts.Build, "15m"), budget(env.Timeouts.ShutdownGrace, "10s"))
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

func generateTraefikLabels(serviceName string, r RouterConfig, defaultResolver string) []string {
	var labels []string
	if r.Enabled != nil && !*r.Enabled {
		return []string{"traefik.enable=false"}
	}
	if r.Domain == "" && r.Host == "" && r.Rule == "" {
		return labels
	}

	labels = append(labels, "traefik.enable=true")

	// High Priority for Main App (beats maintenance page)
	labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.priority=100", serviceName))

	rule := r.Rule
	if rule == "" {
		if r.Domain != "" {
			rule = fmt.Sprintf("Host(`%s`)", r.Domain)
		} else {
			rule = fmt.Sprintf("Host(`%s`)", r.Host)
		}
	}
	labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.rule=%s", serviceName, rule))

	eps := r.EntryPoints
	if len(eps) == 0 {
		eps = []string{"websecure"}
	}
	labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.entrypoints=%s", serviceName, strings.Join(eps, ",")))

	resolver := r.CertResolver
	if resolver == "" {
		resolver = defaultResolver
	}
	if resolver == "" {
		resolver = "myresolver"
	}
	labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.tls.certresolver=%s", serviceName, resolver))

	var mws []string
	if r.StripPrefix && r.PathPrefix != "" {
		mw := serviceName + "-strip"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.stripprefix.prefixes=%s", mw, r.PathPrefix))
		mws = append(mws, mw)
	}

	// New Simplified Auth Flag
	if r.Auth {
		// Convention: We assume the server has a middleware named "global-auth"
		// This is created by 'deploy server provision'
		mws = append(mws, "global-auth")
	}

	// Legacy/Advanced Auth Headers
	if len(r.BasicAuth) > 0 {
		mw := serviceName + "-auth"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.basicauth.users=%s", mw, strings.Join(r.BasicAuth, ",")))
		mws = append(mws, mw)
	}
	if r.BasicAuthFile != "" {
		mw := serviceName + "-authfile"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.basicauth.usersfile=%s", mw, r.BasicAuthFile))
		mws = append(mws, mw)
	}
	if len(r.IPAllowList) > 0 {
		mw := serviceName + "-ip"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.ipallowlist.sourcerange=%s", mw, strings.Join(r.IPAllowList, ",")))
		mws = append(mws, mw)
	}
	if r.RateLimit != nil && r.RateLimit.Average > 0 {
		mw := serviceName + "-rate"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.ratelimit.average=%d", mw, r.RateLimit.Average))
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.ratelimit.burst=%d", mw, r.RateLimit.Burst))
		mws = append(mws, mw)
	}
	if r.Compress {
		mw := serviceName + "-compress"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.compress=true", mw))
		mws = append(mws, mw)
	}
	if len(r.Headers) > 0 {
		mw := serviceName + "-headers"
		keys := make([]string, 0, len(r.Headers))
		for k := range r.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := r.Headers[k]
			labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.headers.customrequestheaders.%s=%s", mw, k, v))
		}
		mws = append(mws, mw)
	}
	if len(mws) > 0 {
		labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.middlewares=%s", serviceName, strings.Join(mws, ",")))
	}

	port := r.InternalPort
	if port == 0 {
		port = 8080
	}
	labels = append(labels, fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port=%d", serviceName, port))
	return labels
}

func doMaintenanceEnable(envName string) {
	_, env := loadEnv(envName)

	// Removed the strict check. If the user invokes this command, they want it.
	// Defaults will be applied in generateMaintenance()

	logInfo("🚧 Setting up Maintenance Page infrastructure on %s...", env.Host)

	if dryRun {
		logInfo("[DRY] Generate, stage and activate maintenance artifacts for %s", env.Quadlet.ServiceName)
		return
	}
	temporary, err := os.MkdirTemp("", "deploy-maintenance-")
	if err != nil {
		logFatal("Maintenance staging: %v", err)
	}
	defer os.RemoveAll(temporary)
	maintPath, htmlPath, err := generateMaintenance(env, temporary)
	if err != nil {
		logFatal("Maintenance generation: %v", err)
	}

	// 2. Sync
	logInfo("📤 Syncing maintenance artifacts...")
	if err := runSSH(env, "mkdir -p -- "+shellQuote(env.Dir+"/maintenance")+" ~/.config/containers/systemd"); err != nil {
		logFatal("Maintenance directory: %v", err)
	}
	runRsync(env, []string{htmlPath}, fmt.Sprintf("%s@%s:%s/maintenance/index.html", env.User, env.Host, env.Dir))
	runRsync(env, []string{maintPath}, remoteDestination(env, ".config/containers/systemd/"))

	// 3. Activate
	serviceName := env.Quadlet.ServiceName + "-maint"
	logInfo("🔄 Activating maintenance container (%s)...", serviceName)

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

	if err := runSSH(env, script); err != nil {
		logFatal("Failed to start maintenance container: %v", err)
	}

	logSuccess("✅ Maintenance page is UP (Priority 1).")
	logInfo("   It will be served automatically whenever '%s' (Priority 100) is stopped.", env.Quadlet.ServiceName)
}

func doMaintenanceDisable(envName string) {
	_, env := loadEnv(envName)
	serviceName := env.Quadlet.ServiceName + "-maint"

	logInfo("🚧 Disabling Maintenance Page on %s...", env.Host)

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

	if err := runSSH(env, script); err != nil {
		logFatal("Failed to disable maintenance page: %v", err)
	}

	// Optional: We leave the HTML file in place as it's harmless and useful for future enables.
	logSuccess("✅ Maintenance page disabled and removed.")
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
	if dryRun {
		if explicitVersion == "" {
			return "<configuration-version>", nil
		}
		return explicitVersion, nil
	}
	out, err := gitOutput("status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("git status: %w", err)
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return "", fmt.Errorf("Git working directory is dirty; commit or stash changes before releasing")
	}
	hasRemote := gitRun("remote", "get-url", "origin") == nil
	if explicitVersion != "" {
		// Accept tags only, never revision expressions or command-line options.
		if err := gitRun("check-ref-format", "refs/tags/"+explicitVersion); err != nil {
			return "", fmt.Errorf("invalid Git tag")
		}
		head, err := gitOutput("rev-parse", "--verify", "HEAD")
		if err != nil {
			return "", err
		}
		tag, err := gitOutput("rev-parse", "--verify", "refs/tags/"+explicitVersion+"^{commit}")
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
	if tag, err := gitOutput("describe", "--tags", "--exact-match", "HEAD"); err == nil {
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
	if err := runCommandRaw("git", "--no-pager", "tag", "--sort=-v:refname", "--list"); err != nil {
		return "", err
	}
	version := prompt("Enter new semantic version (e.g. v1.0.1)")
	if err := validateReleaseVersion(version); err != nil {
		return "", err
	}
	if version == "" || strings.HasPrefix(version, "-") {
		return "", fmt.Errorf("valid version tag required")
	}
	if err := gitRun("check-ref-format", "refs/tags/"+version); err != nil {
		return "", fmt.Errorf("invalid Git tag")
	}
	if err := runCommandRaw("git", "tag", "-a", version, "-m", "Release "+version); err != nil {
		return "", err
	}
	if hasRemote {
		if err := runCommandRaw("git", "push", "origin", "refs/tags/"+version); err != nil {
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
	if !confirm(fmt.Sprintf("Push tag %s to origin?", version)) {
		return fmt.Errorf("deployment requires synced Git tags")
	}
	if err := runCommandRaw("git", "push", "origin", "refs/tags/"+version); err != nil {
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
	if err := gitRun("check-ref-format", ref); err != nil {
		return false, fmt.Errorf("invalid Git tag")
	}
	local, err := gitOutput("rev-parse", "--verify", ref)
	if err != nil {
		return false, fmt.Errorf("local tag %s: %w", version, err)
	}
	peeled, err := gitOutput("rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return false, fmt.Errorf("tag %s must identify a commit: %w", version, err)
	}
	out, err := gitOutput("ls-remote", "--tags", "origin", ref, ref+"^{}")
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

func getBuildMetadata(explicitVersion string) BuildMetadata {
	get := func(args ...string) string {
		if dryRun {
			return "dry"
		}
		out, _ := boundedOutput(exec.Command(args[0], args[1:]...), 2*time.Minute)
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

	return BuildMetadata{
		Version:     v,
		Date:        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Tag:         v,
		Commit:      get("git", "rev-parse", "HEAD"),
		MainVersion: mainVer,
		GoVersion:   runtime.Version(),
	}
}

func generateMaintenance(env Environment, outDir string) (string, string, error) {
	// 1. Apply Defaults if config is missing (Enabled check removed in CLI)
	if env.Maintenance.Title == "" {
		env.Maintenance.Title = "System Maintenance"
	}
	if env.Maintenance.Text == "" {
		env.Maintenance.Text = "The system is currently undergoing maintenance. We will be back shortly."
	}

	// 2. Generate HTML
	var htmlBuf bytes.Buffer
	t1, err := template.New("h").Parse(maintenanceHtmlTmpl)
	if err != nil {
		return "", "", err
	}
	if err = t1.Execute(&htmlBuf, env.Maintenance); err != nil {
		return "", "", err
	}

	htmlPath := filepath.Join(outDir, "maintenance.html")
	if !dryRun {
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

	data := MaintenanceTemplateData{
		ServiceName: env.Quadlet.ServiceName,
		Rule:        rule,
		Network:     env.Quadlet.Network,
		TargetDir:   env.Dir,
		Resolver:    resolver,
	}
	var cBuf bytes.Buffer
	t2, err := template.New("mc").Parse(maintenanceContainerTmpl)
	if err != nil {
		return "", "", err
	}
	if err = t2.Execute(&cBuf, data); err != nil {
		return "", "", err
	}

	containerPath := filepath.Join(outDir, env.Quadlet.ServiceName+"-maint.container")
	if !dryRun {
		if err = os.WriteFile(containerPath, cBuf.Bytes(), 0644); err != nil {
			return "", "", err
		}
	}

	return containerPath, htmlPath, nil
}
