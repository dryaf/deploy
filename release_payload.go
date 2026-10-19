package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Release payloads have private paths and outlive the shared build-output lock.
type releasePayload struct {
	Dir, Binary, Container, Env string
	active                      *os.File
}

func selectedArtifacts(n NormalizedConfig) []string {
	if n.Mode == ModeBinary && len(n.Config.Artifacts.Include) == 0 {
		return []string{"Dockerfile.vps", "migrations", "files"}
	}
	return n.Config.Artifacts.Include
}

func captureRelease(p ReleasePlan, binary string) (*releasePayload, error) {
	n := p.Config
	cleanupAbandonedPayloads()
	paths := append([]string(nil), selectedArtifacts(n)...)
	if n.Env.SyncEnvFile != "" {
		paths = append(paths, n.Env.SyncEnvFile)
	}
	if binary != "" {
		paths = append(paths, binary)
	}
	before, err := inputFingerprint(paths)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "deploy-payload-")
	if err != nil {
		return nil, err
	}
	active, err := os.OpenFile(filepath.Join(dir, ".active.lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if _, err = active.WriteString("deploy-payload-v1\n"); err != nil {
		active.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	if err = syscall.Flock(int(active.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		active.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			active.Close()
			removePrivatePayload(dir)
		}
	}()
	if err := os.Mkdir(filepath.Join(dir, "artifacts"), 0700); err != nil {
		return nil, err
	}
	if artifacts := selectedArtifacts(n); len(artifacts) > 0 {
		args := append([]string{"-a", "--relative"}, excludeArgs(n.Config.Artifacts.Exclude)...)
		args = append(args, "--")
		args = append(args, cleanArtifactSources(artifacts)...)
		args = append(args, filepath.Join(dir, "artifacts")+"/")
		if out, err := boundedOutput(exec.Command("rsync", args...), budget(n.Env.Timeouts.Transfer, "10m")); err != nil {
			return nil, fmt.Errorf("capture artifacts: %w: %s", err, out)
		}
	}
	payload := &releasePayload{Dir: dir, Container: filepath.Join(dir, "candidate.container"), active: active}
	if err := os.WriteFile(payload.Container, []byte(p.Quadlet), 0600); err != nil {
		return nil, err
	}
	if binary != "" {
		payload.Binary = filepath.Join(dir, "artifacts", n.Config.BinaryName)
		if err := copyRegularFile(binary, payload.Binary); err != nil {
			return nil, err
		}
	}
	if n.Env.SyncEnvFile != "" {
		payload.Env = filepath.Join(dir, "env")
		if err := copyRegularFile(n.Env.SyncEnvFile, payload.Env); err != nil {
			return nil, err
		}
		if err := os.Chmod(payload.Env, 0600); err != nil {
			return nil, err
		}
	}
	after, err := inputFingerprint(paths)
	if err != nil {
		return nil, err
	}
	if before != after {
		return nil, fmt.Errorf("release inputs changed during capture; retry from a stable checkout")
	}
	if p.CheckSource {
		tag, err := gitOutput("rev-parse", "--verify", "refs/tags/"+p.Version)
		if err != nil || strings.TrimSpace(string(tag)) != p.TagObject {
			return nil, fmt.Errorf("release tag changed during build")
		}
		head, err := gitOutput("rev-parse", "HEAD")
		if err != nil || strings.TrimSpace(string(head)) != p.Commit {
			return nil, fmt.Errorf("Git HEAD changed during release")
		}
		status, err := gitOutput("status", "--porcelain", "--untracked-files=no")
		if err != nil || len(strings.TrimSpace(string(status))) > 0 {
			return nil, fmt.Errorf("tracked source changed during release; commit changes and retry")
		}
		if gitRun("remote", "get-url", "origin") == nil {
			exists, err := verifyRemoteTag(p.Version)
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, fmt.Errorf("remote release tag disappeared")
			}
		}
	}
	if n.Mode == ModeBinary {
		info, err := os.Lstat(filepath.Join(dir, "artifacts", n.Env.Quadlet.Dockerfile))
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("Dockerfile must be included in the captured artifacts")
		}
	}
	// Enumerate the captured tree only. sha256sum runs relative to the staging
	// directory remotely, so it never verifies a path outside the release payload.
	var sums strings.Builder
	err = filepath.WalkDir(dir, func(file string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || file == filepath.Join(dir, ".active.lock") {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("unsupported captured path: %s", file)
		}
		rel, err := filepath.Rel(dir, file)
		if err != nil {
			return err
		}
		if strings.ContainsAny(rel, "\n\r\\") {
			return fmt.Errorf("unsupported payload filename")
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%x  %s\n", h.Sum(nil), filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(dir, "payload.sha256"), []byte(sums.String()), 0600); err != nil {
		return nil, err
	}
	ok = true
	return payload, nil
}

func inputFingerprint(paths []string) (string, error) {
	h := sha256.New()
	for _, root := range paths {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		for current := absolute; current != filepath.Dir(current); current = filepath.Dir(current) {
			info, err := os.Lstat(current)
			if err != nil {
				return "", err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("symlink in release input: %s", root)
			}
		}
		err = filepath.WalkDir(root, func(file string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\x00%o\x00", file, info.Mode())
			if d.IsDir() {
				return nil
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("unsupported release input: %s", file)
			}
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			_, err = io.Copy(h, f)
			f.Close()
			return err
		})
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func copyRegularFile(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("expected regular file: %s", source)
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	// Reject a captured artifact directory or symlink occupying the binary name.
	if old, err := os.Lstat(destination); err == nil {
		if !old.Mode().IsRegular() {
			return fmt.Errorf("unsafe payload destination: %s", destination)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Chmod(info.Mode().Perm())
	}
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func (p *releasePayload) Close() {
	if p == nil {
		return
	}
	_ = removePrivatePayload(p.Dir)
	if p.active != nil {
		_ = p.active.Close()
	}
}

func cleanupAbandonedPayloads() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "deploy-payload-") {
			continue
		}
		dir := filepath.Join(os.TempDir(), entry.Name())
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || time.Since(info.ModTime()) < 24*time.Hour {
			continue
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Getuid()) {
			continue
		}
		f, err := os.OpenFile(filepath.Join(dir, ".active.lock"), os.O_RDWR|syscall.O_NOFOLLOW, 0)
		if err != nil {
			continue
		}
		header := make([]byte, len("deploy-payload-v1\n"))
		_, readErr := io.ReadFull(f, header)
		if readErr == nil && string(header) == "deploy-payload-v1\n" && syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			_ = removePrivatePayload(dir)
		}
		_ = f.Close()
	}
}

func removePrivatePayload(dir string) error {
	if filepath.Dir(dir) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(dir), "deploy-payload-") {
		return fmt.Errorf("unsafe private payload cleanup path")
	}
	if info, err := os.Lstat(dir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("payload cleanup requires a real directory")
	}
	if err := filepath.WalkDir(dir, func(file string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.Chmod(file, info.Mode().Perm()|0700)
		}
		return nil
	}); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
