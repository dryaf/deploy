package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
)

//go:embed image_transaction.sh
var imageTransactionBody string

//go:embed transaction_common.sh
var transactionCommonScript string

var imageTransactionScript = transactionCommonScript + "\n" + imageTransactionBody

type imageReleaseRequest struct {
	Schema                int             `json:"schema"`
	ReleaseID             string          `json:"release_id"`
	Version               string          `json:"git_version"`
	Commit                string          `json:"git_commit"`
	TagObject             string          `json:"git_tag_object,omitempty"`
	PayloadChecksums      bool            `json:"payload_checksums,omitempty"`
	Service               string          `json:"service"`
	Target                string          `json:"target"`
	Reference             string          `json:"configured_reference"`
	EnvFile               string          `json:"env_file"`
	SyncEnv               bool            `json:"sync_env"`
	HealthCommand         bool            `json:"health_command"`
	HealthURL             string          `json:"health_url"`
	HealthSeconds         float64         `json:"health_seconds"`
	HealthStableSeconds   float64         `json:"health_stable_seconds,omitempty"`
	HealthIntervalSeconds float64         `json:"health_interval_seconds,omitempty"`
	HealthSuccesses       int             `json:"health_successes,omitempty"`
	Timeouts              operationLimits `json:"timeouts"`
	MinFreeBytes          int64           `json:"min_free_bytes,omitempty"`
	MinFreeInodes         int64           `json:"min_free_inodes,omitempty"`
	ImageReserveBytes     int64           `json:"image_reserve_bytes,omitempty"`
	Stop                  bool            `json:"stop_on_deploy"`
	Environment           string          `json:"environment"`
	Database              string          `json:"database,omitempty"`
	DatabaseRollback      string          `json:"database_rollback,omitempty"`
	UID                   int             `json:"container_uid"`
	GID                   int             `json:"container_gid"`
}

func executeImageRelease(p ReleasePlan, runner ReleaseRunner) error {
	lock := p.projectLock
	if lock == nil {
		var err error
		lock, err = lockProject()
		if err != nil {
			return err
		}
	}
	defer lock.Close()
	payload, err := captureRelease(p, "")
	if err != nil {
		return err
	}
	defer payload.Close()
	lock.Close()
	n, e := p.Config, p.Config.Env
	if err := checkLocalReleaseTools(); err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "deploy-image-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	request := imageReleaseRequest{
		Schema: 1, ReleaseID: p.ReleaseID, Version: p.Version, Commit: p.Commit, Service: e.Quadlet.ServiceName,
		Target: e.Dir, Reference: e.Quadlet.Image, EnvFile: n.EnvFile, SyncEnv: e.SyncEnvFile != "",
		HealthCommand: e.Quadlet.HealthCmd != "", HealthURL: e.Quadlet.HealthURL,
		HealthSeconds: n.HealthTimeout.Seconds(), Stop: e.Quadlet.StopOnDeploy,
	}
	configureReleaseDatabase(&request, e)
	configureReleasePolicy(&request, n)
	request.TagObject = p.TagObject
	request.PayloadChecksums = true
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	for name, content := range map[string][]byte{"request.json": body, "candidate.container": []byte(p.Quadlet), "transaction.sh": []byte(imageTransactionScript)} {
		if err := os.WriteFile(filepath.Join(temp, name), content, 0600); err != nil {
			return err
		}
	}
	stage := path.Join(e.Dir, ".deploy/image/staging", p.ReleaseID)
	// This preflight does not modify the service or its active files.
	setup := imageRemotePreflight + "\n" + imageResourceChecks(e.Quadlet)
	if request.Database != "" {
		setup += databaseReleasePreflight
	}
	if e.Quadlet.HealthURL != "" {
		setup += "command -v curl >/dev/null || { echo 'Host health_url requires curl' >&2; exit 1; }\n"
	}
	setup += safeRemoteMkdir(stage) + "\n" + safeRemoteMkdir(path.Join(stage, "artifacts"))
	if _, err = runner.SSH(e, setup); err != nil {
		return fmt.Errorf("prepare image release: %w", err)
	}
	sources := []string{filepath.Join(temp, "request.json"), filepath.Join(temp, "candidate.container"), filepath.Join(temp, "transaction.sh")}
	if err = runner.Sync(e, sources, remoteDestination(e, stage+"/")); err != nil {
		return fmt.Errorf("stage release: %w", err)
	}
	if err = runner.Sync(e, []string{payload.Dir + "/"}, remoteDestination(e, stage+"/"), "--checksum", "--exclude=/.active.lock"); err != nil {
		return fmt.Errorf("stage captured payload: %w", err)
	}
	out, err := runner.SSH(e, "bash "+shellQuote(stage+"/transaction.sh")+" release "+shellQuote(stage))
	if out != "" {
		fmt.Print(out)
	}
	if err != nil {
		return fmt.Errorf("image deployment: %w", err)
	}
	return nil
}

// Walk components instead of following symlinks into a different project or backup tree.
func safeRemoteMkdir(dir string) string {
	return "safe_mkdir() { local p=/ part; local -a parts; IFS=/ read -ra parts <<< \"$1\"; for part in \"${parts[@]}\"; do [ -n \"$part\" ] || continue; p=\"${p%/}/$part\"; [ ! -L \"$p\" ] || { echo 'Symlink in deployment path' >&2; return 1; }; if [ ! -d \"$p\" ]; then mkdir -m 700 -- \"$p\" || return; fi; done; }; safe_mkdir " + shellQuote(dir)
}

const imageRemotePreflight = `set -euo pipefail
umask 077
for tool in bash podman systemctl rsync flock jq timeout realpath find stat cmp sha256sum grep sed sort cut dirname date cp mv chmod mkdir rm rmdir awk id sync cat du; do
 command -v "$tool" >/dev/null || { echo "Missing remote prerequisite: $tool" >&2; exit 1; }
done
version=$(podman version --format '{{.Client.Version}}')
IFS=. read -r major minor patch <<< "$version"
patch=${patch%%[^0-9]*}
(( major > 5 || (major == 5 && (minor > 4 || (minor == 4 && ${patch:-0} >= 2))) )) || { echo 'Podman >= 5.4.2 is required' >&2; exit 1; }
systemd_version=$(systemctl --version | awk 'NR == 1 {print $2}')
(( systemd_version >= 257 )) || { echo 'systemd >= 257 is required' >&2; exit 1; }
[ "$(podman info --format '{{.Host.Security.Rootless}}')" = true ] || { echo 'Rootless Podman is required' >&2; exit 1; }
[ "$(podman info --format '{{.Host.CgroupsVersion}}')" = v2 ] || { echo 'cgroup v2 is required' >&2; exit 1; }
systemctl --user show-environment >/dev/null
[ -x /usr/lib/systemd/system-generators/podman-system-generator ] || [ -x /usr/libexec/podman/quadlet ] || { echo 'Quadlet generator missing' >&2; exit 1; }
`

func imageResourceChecks(q Quadlet) string {
	if q.CPU == "" && q.Memory == "" {
		return ""
	}
	script := `controllers=$(podman info --format '{{json .Host.CgroupControllers}}')
`
	if q.CPU != "" {
		script += `jq -e 'index("cpu") != null' <<< "$controllers" >/dev/null || { echo 'Requested CPU limit requires a delegated cgroup cpu controller' >&2; exit 1; }
`
	}
	if q.Memory != "" {
		script += `jq -e 'index("memory") != null' <<< "$controllers" >/dev/null || { echo 'Requested memory limit requires a delegated cgroup memory controller' >&2; exit 1; }
`
	}
	return script
}

func executeImageServiceAction(n NormalizedConfig, action string, runner ReleaseRunner) error {
	return executeDeploymentServiceAction(n, action, runner, imageTransactionScript)
}

func executeDeploymentServiceAction(n NormalizedConfig, action string, runner ReleaseRunner, script string) error {
	// Service mutations recover pending transactions under both deployment locks.
	p, err := newReleaseID("service-action")
	if err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "deploy-image-action-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	e := n.Env
	req := imageReleaseRequest{Schema: 1, ReleaseID: p, Service: e.Quadlet.ServiceName, Target: e.Dir}
	req.Timeouts = limitsFor(e)
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{"request.json": body, "transaction.sh": []byte(script)} {
		if err := os.WriteFile(filepath.Join(temp, name), data, 0600); err != nil {
			return err
		}
	}
	stage := path.Join(e.Dir, ".deploy", string(n.Mode), "staging", p)
	if _, err := runner.SSH(e, "set -euo pipefail\numask 077\n"+safeRemoteMkdir(stage)); err != nil {
		return err
	}
	if err := runner.Sync(e, []string{filepath.Join(temp, "request.json"), filepath.Join(temp, "transaction.sh")}, remoteDestination(e, stage+"/")); err != nil {
		return err
	}
	out, err := runner.SSH(e, "bash "+shellQuote(stage+"/transaction.sh")+" action "+shellQuote(stage)+" "+shellQuote(action))
	if out != "" {
		fmt.Print(out)
	}
	return err
}

func imageStatusScript(n NormalizedConfig) string {
	state := path.Join(n.Env.Dir, ".deploy", string(n.Mode))
	return "state=" + shellQuote(state) + `;
if [ -f "$state/current.json" ]; then
 id=$(jq -er .current "$state/current.json") || exit
 case "$id" in *[!a-zA-Z0-9_-]*|'') echo 'Invalid current release ID' >&2; exit 1;; esac
 actual=$(podman container inspect --format '{{.Image}}' ` + shellQuote("systemd-"+n.Env.Quadlet.ServiceName) + ` 2>/dev/null) || actual=''
 if [[ $actual =~ ^(sha256:)?[0-9a-f]{64}$ ]]; then actual="sha256:${actual#sha256:}"; else actual=''; fi
 jq --arg actual "$actual" '{release_id,git_version,git_commit,git_tag_object,configured_reference,image_id,registry_digests,check_result,running_image_id:(if $actual == "" then null else $actual end),image_matches:($actual == .image_id)}' "$state/releases/$id/manifest.json"
else echo 'No confirmed deployment'; fi
if [ -f "$state/pending.json" ]; then echo 'PENDING RECOVERY:'; jq '{schema,release_id}' "$state/pending.json"; else echo 'Pending recovery: none'; fi
if [ -f "$state/boot.json" ]; then jq '{enabled}' "$state/boot.json"; fi
if [ -f "$state/boot-pending.json" ]; then echo 'PENDING BOOT RECOVERY:'; jq '{schema,operation_id}' "$state/boot-pending.json"; fi`
}
