package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type ReleasePlan struct {
	Config    NormalizedConfig
	Version   string
	Commit    string
	ReleaseID string
	Quadlet   string
	Steps     []string
}

// Runner is the process/transport boundary. Planning never calls it.
type ReleaseRunner interface {
	SSH(Environment, string) (string, error)
	Sync(Environment, []string, string, ...string) error
	Local(*exec.Cmd) error
}
type commandRunner struct{}

func (commandRunner) SSH(e Environment, s string) (string, error) { return runSSHOutput(e, s) }
func (commandRunner) Sync(e Environment, s []string, d string, a ...string) error {
	return runRsyncSafe(e, s, d, a...)
}
func (commandRunner) Local(c *exec.Cmd) error { return runCommand("local command", c) }

func planImageRelease(n NormalizedConfig, version, commit string) (ReleasePlan, error) {
	p := ReleasePlan{Config: n, Version: version, Commit: commit}
	if n.Mode != ModeImage {
		return p, fmt.Errorf("image plan requires image mode")
	}
	if err := checkLocalImageArtifacts(n); err != nil {
		return p, err
	}
	var err error
	p.ReleaseID, err = newReleaseID(version)
	if err != nil {
		return p, err
	}
	p.Quadlet, err = renderQuadlet(n, imagePlaceholder)
	if err != nil {
		return p, err
	}
	p.Steps = []string{
		"Validate local Git, SSH and rsync; verify remote Podman >= 5.4.2, systemd >= 257, cgroup v2, rootless systemd, rsync, flock, jq, timeout and Quadlet generator",
		"Stage only explicit artifacts and optional synchronized .env in a private release directory",
		"Acquire per-user/service lock; recover pending transaction; reject unmanaged Quadlets or state drift",
		"Snapshot previous Quadlet, exact image ID, service state and managed files; retain previous image",
		"Pull " + n.Env.Quadlet.Image + " and validate the returned image ID and native platform",
		"Pin candidate Image=<resolved-image-id>, Pull=never; retain image and validate using isolated Quadlet generator",
		"Persist pending manifest; install managed files and atomically replace Quadlet; daemon-reload and restart",
		fmt.Sprintf("Wait up to %s for active service, exact image and configured container/host health checks", n.HealthTimeout),
		"Atomically commit current and previous release pointers; remove pending marker",
		"On activation failure restore previous Quadlet, exact image, managed files, commit pointers and original activity; retain pending on rollback failure",
	}
	return p, nil
}

func newReleaseID(version string) (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	versionSlug := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, version)
	versionSlug = strings.Trim(versionSlug, "-_")
	if len(versionSlug) > 40 {
		versionSlug = versionSlug[:40]
	}
	if versionSlug == "" {
		versionSlug = "release"
	}
	return versionSlug + "-" + time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(random), nil
}

func planBinaryRelease(n NormalizedConfig, version, commit string) (ReleasePlan, error) {
	p := ReleasePlan{Config: n, Version: version, Commit: commit}
	var err error
	p.Quadlet, err = renderQuadlet(n, "")
	p.Steps = []string{"Build Go binary (or build.cmd)", "Stage binary, artifacts, optional .env and Quadlet before changing the service", "Acquire service and target locks; recover pending state; snapshot managed files, Quadlet, image and activity", "Install files, build and pin remote image, daemon-reload and restart", "Verify readiness and commit; restore the exact previous image, configuration, files and activity on failure"}
	return p, err
}

func (p ReleasePlan) Print(w io.Writer) {
	n := p.Config
	fmt.Fprintf(w, "Mode: %s\nConfiguration version: %s\nService: %s\nTarget: %s@%s:%s\nImage reference: %s\nEnvironment file: %s\nRouter enabled: %t\nCPU quota: %s; memory: %s; restart on failure: %t\n", n.Mode, p.Version, n.Env.Quadlet.ServiceName, n.Env.User, n.Env.Host, n.Env.Dir, n.Env.Quadlet.Image, displayDefault(n.EnvFile, "(none)"), n.Routing, displayDefault(n.CPUQuota, "(unlimited)"), displayDefault(n.Env.Quadlet.Memory, "(unlimited)"), n.Env.Quadlet.AutoRestart)
	if n.Mode == ModeImage {
		fmt.Fprintf(w, "Resolved image: <resolved-image-id> (available after remote pull)\nArtifacts: %v\nNetwork: %s; aliases: %v\n", n.Config.Artifacts.Include, n.Env.Quadlet.Network, n.Env.Quadlet.NetworkAliases)
	}
	for i, s := range p.Steps {
		fmt.Fprintf(w, "%d. %s\n", i+1, s)
	}
	if n.Env.Quadlet.HealthCmd == "" && n.Env.Quadlet.HealthURL == "" {
		fmt.Fprintln(w, "Readiness: service activity and image identity only; no application health check configured")
	}
	if n.Env.Database.Driver == "sqlite" {
		fmt.Fprintf(w, "Database: %s; stop service and retain snapshot before startup; changed-database rollback: %s\n", n.Env.Database.Source, displayDefault(n.Env.Database.Rollback, "manual"))
	}
}
func displayDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func checkLocalImageArtifacts(n NormalizedConfig) error {
	paths := append([]string(nil), n.Config.Artifacts.Include...)
	if n.Env.SyncEnvFile != "" {
		paths = append(paths, n.Env.SyncEnvFile)
	}
	for _, p := range paths {
		absolute, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		// Reject symlinks in every component, including parents of explicitly selected files.
		for current := absolute; current != filepath.Dir(current); current = filepath.Dir(current) {
			s, err := os.Lstat(current)
			if err != nil {
				return fmt.Errorf("local artifact %q: %w", p, err)
			}
			if s.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink in local artifact %q", p)
			}
		}
		if err := filepath.WalkDir(p, func(file string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink artifact %q", file)
			}
			if !d.IsDir() && !d.Type().IsRegular() {
				return fmt.Errorf("unsupported artifact type %q", file)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if n.Env.SyncEnvFile != "" {
		s, err := os.Stat(n.Env.SyncEnvFile)
		if err != nil {
			return err
		}
		if !s.Mode().IsRegular() {
			return fmt.Errorf("sync_env_file must be a regular file")
		}
	}
	return nil
}

func checkLocalReleaseTools() error {
	names := []string{"git", "ssh", "rsync"}

	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("local prerequisite %s missing", name)
		}
	}
	return nil
}

func remoteDestination(e Environment, p string) string {
	host := e.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return e.User + "@" + host + ":" + p
}
func excludeArgs(excludes []string) []string {
	var a []string
	for _, s := range excludes {
		a = append(a, "--exclude="+s)
	}
	return a
}
func cleanArtifactSources(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, "./"+strings.TrimPrefix(filepath.Clean(p), "./"))
	}
	return out
}
