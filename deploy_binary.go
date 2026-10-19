package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
)

//go:embed binary_transaction.sh
var binaryTransactionBody string

var binaryTransactionScript = transactionCommonScript + "\n" + binaryTransactionBody

type binaryReleaseRequest struct {
	imageReleaseRequest
	Dockerfile string   `json:"dockerfile"`
	ChownPaths []string `json:"chown_paths"`
}

const binaryRemotePreflight = `set -euo pipefail
umask 077
for tool in bash podman systemctl rsync jq flock sync timeout realpath find stat cmp sha256sum grep sed sort cut dirname cp mv chmod mkdir rm rmdir cat chown date; do
 command -v "$tool" >/dev/null || { echo "Missing remote prerequisite: $tool" >&2; exit 1; }
done
systemctl --user show-environment >/dev/null
`

func stageBinaryRelease(p ReleasePlan, containerPath string, runner ReleaseRunner) error {
	n, e := p.Config, p.Config.Env
	temp, err := os.MkdirTemp("", "deploy-binary-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	id, err := newReleaseID(p.Version)
	if err != nil {
		return err
	}
	req := binaryReleaseRequest{imageReleaseRequest: imageReleaseRequest{Schema: 1, ReleaseID: id, Version: p.Version, Commit: p.Commit, Service: e.Quadlet.ServiceName, Target: e.Dir, Reference: e.Quadlet.Image, EnvFile: n.EnvFile, SyncEnv: e.SyncEnvFile != "", HealthCommand: e.Quadlet.HealthCmd != "", HealthURL: e.Quadlet.HealthURL, HealthSeconds: n.HealthTimeout.Seconds(), Stop: e.Quadlet.StopOnDeploy}, Dockerfile: e.Quadlet.Dockerfile}
	configureReleaseDatabase(&req.imageReleaseRequest, e)
	for _, owned := range e.Quadlet.ChownVolumes {
		if !path.IsAbs(owned) {
			owned = path.Join(e.Dir, owned)
		}
		req.ChownPaths = append(req.ChownPaths, path.Clean(owned))
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{"request.json": body, "transaction.sh": []byte(binaryTransactionScript)} {
		if err := os.WriteFile(filepath.Join(temp, name), data, 0600); err != nil {
			return err
		}
	}
	stage := path.Join(e.Dir, ".deploy/binary/staging", id)
	setup := "set -euo pipefail\numask 077\n" + safeRemoteMkdir(stage) + "\n" + safeRemoteMkdir(stage+"/artifacts")
	// Persistent data is excluded from artifacts but required by the default bind mount and chown_volumes.
	setup += "\n" + safeRemoteMkdir(path.Join(e.Dir, "data"))
	if _, err := runner.SSH(e, setup); err != nil {
		return err
	}
	if err := runner.Sync(e, []string{filepath.Join(temp, "request.json"), filepath.Join(temp, "transaction.sh")}, remoteDestination(e, stage+"/")); err != nil {
		return err
	}
	if err := runner.Sync(e, []string{containerPath}, remoteDestination(e, stage+"/candidate.container")); err != nil {
		return err
	}
	artifacts := n.Config.Artifacts.Include
	if len(artifacts) == 0 {
		artifacts = []string{"Dockerfile.vps", "migrations", "files"}
	}
	args := append([]string{"--relative"}, excludeArgs(n.Config.Artifacts.Exclude)...)
	if err := runner.Sync(e, cleanArtifactSources(artifacts), remoteDestination(e, stage+"/artifacts/"), args...); err != nil {
		return fmt.Errorf("stage binary artifacts: %w", err)
	}
	// Artifact filters must never omit the compiled binary. Upload it last so a
	// same-name artifact cannot replace it, even with identical size and mtime.
	if err := runner.Sync(e, []string{filepath.Join("build", n.Config.BinaryName)}, remoteDestination(e, stage+"/artifacts/"), "--checksum"); err != nil {
		return fmt.Errorf("stage built binary: %w", err)
	}
	if e.SyncEnvFile != "" {
		if err := runner.Sync(e, []string{e.SyncEnvFile}, remoteDestination(e, stage+"/env"), "--chmod=F600"); err != nil {
			return err
		}
	}
	out, err := runner.SSH(e, "bash "+shellQuote(stage+"/transaction.sh")+" release "+shellQuote(stage))
	if out != "" {
		fmt.Print(out)
	}
	return err
}
