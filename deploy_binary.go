package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

//go:embed binary_transaction.sh
var binaryTransactionBody string

var binaryTransactionScript = transactionCommonScript + "\n" + binaryTransactionBody

type binaryReleaseRequest struct {
	imageReleaseRequest
	Dockerfile string   `json:"dockerfile"`
	UID        int      `json:"container_uid"`
	GID        int      `json:"container_gid"`
	ChownPaths []string `json:"chown_paths"`
}

const binaryRemotePreflight = `set -euo pipefail
umask 077
for tool in bash podman systemctl rsync jq flock sync timeout realpath find stat cmp sha256sum grep sed sort cut dirname cp mv chmod mkdir rm rmdir cat chown; do
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
	req := binaryReleaseRequest{imageReleaseRequest: imageReleaseRequest{Schema: 1, ReleaseID: id, Version: p.Version, Commit: p.Commit, Service: e.Quadlet.ServiceName, Target: e.Dir, Reference: e.Quadlet.Image, EnvFile: n.EnvFile, SyncEnv: e.SyncEnvFile != "", HealthCommand: e.Quadlet.HealthCmd != "", HealthURL: e.Quadlet.HealthURL, HealthSeconds: n.HealthTimeout.Seconds(), Stop: e.Quadlet.StopOnDeploy}, Dockerfile: e.Quadlet.Dockerfile, UID: e.Quadlet.ContainerUID, GID: e.Quadlet.ContainerGID}
	for _, owned := range e.Quadlet.ChownVolumes {
		if strings.HasPrefix(owned, "./") {
			owned = path.Join(e.Dir, owned)
		}
		req.ChownPaths = append(req.ChownPaths, owned)
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
	if _, err := runner.SSH(e, "set -euo pipefail\numask 077\n"+safeRemoteMkdir(stage)+"\n"+safeRemoteMkdir(stage+"/artifacts")); err != nil {
		return err
	}
	if err := runner.Sync(e, []string{filepath.Join(temp, "request.json"), filepath.Join(temp, "transaction.sh")}, remoteDestination(e, stage+"/")); err != nil {
		return err
	}
	if err := runner.Sync(e, []string{containerPath}, remoteDestination(e, stage+"/candidate.container")); err != nil {
		return err
	}
	artifacts := []string{"build/" + n.Config.BinaryName}
	if len(n.Config.Artifacts.Include) > 0 {
		artifacts = append(artifacts, n.Config.Artifacts.Include...)
	} else {
		artifacts = append(artifacts, "Dockerfile.vps", "migrations", "files")
	}
	if err := runner.Sync(e, artifacts, remoteDestination(e, stage+"/artifacts/"), excludeArgs(n.Config.Artifacts.Exclude)...); err != nil {
		return fmt.Errorf("stage binary artifacts: %w", err)
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
