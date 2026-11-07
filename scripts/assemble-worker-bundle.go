//go:build ignore

// Assemble already-built local inputs. No downloads, image pushes or provider calls.
package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dryaf/deploy/internal/workerctl"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func artifact(path string) (workerctl.Artifact, error) {
	f, err := os.Open(path)
	if err != nil {
		return workerctl.Artifact{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 512<<20 {
		return workerctl.Artifact{}, fmt.Errorf("artifact must be a bounded regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (512<<20)+1))
	if err != nil || n != info.Size() {
		return workerctl.Artifact{}, fmt.Errorf("artifact changed while hashing")
	}
	return workerctl.Artifact{File: path, SHA256: fmt.Sprintf("%x", h.Sum(nil))}, nil
}
func run() error {
	release := os.Getenv("WORKER_RELEASE")
	if release == "" {
		release = "development"
	}
	c := workerctl.Catalog{Version: 1, Release: release, Linux: map[string]workerctl.ArchitectureArtifacts{}}
	for _, arch := range []string{"amd64", "arm64"} {
		helper, err := artifact(filepath.Join("dist/backup-worker", "deploy-linux-"+arch))
		if err != nil {
			return err
		}
		a := workerctl.ArchitectureArtifacts{Helper: helper, BackupImage: os.Getenv("WORKER_BACKUP_IMAGE_" + strings.ToUpper(arch)), RelayImage: os.Getenv("WORKER_RELAY_IMAGE_" + strings.ToUpper(arch))}
		for _, v := range []struct {
			prefix string
			dest   **workerctl.Artifact
		}{{"WORKER_SIGNAL_", &a.Signal}, {"WORKER_LEDS_", &a.LEDs}} {
			if file := os.Getenv(v.prefix + strings.ToUpper(arch)); file != "" {
				item, err := artifact(file)
				if err != nil {
					return err
				}
				*v.dest = &item
			}
		}
		c.Linux[arch] = a
	}
	complete := os.Getenv("WORKER_BUNDLE_COMPLETE")
	if complete != "" && complete != "0" && complete != "1" {
		return fmt.Errorf("WORKER_BUNDLE_COMPLETE must be 0 or 1")
	}
	_, missing, err := workerctl.AssembleBundle("dist/workers", c, complete == "1")
	if err != nil {
		return err
	}
	fmt.Println("Local worker bundle: dist/workers/catalog.json")
	if len(missing) > 0 {
		fmt.Println("Partial development bundle; missing:", strings.Join(missing, ", "))
	} else {
		fmt.Println("All packaging inputs present. Live, hardware and VM qualification remain separate.")
	}
	return nil
}
