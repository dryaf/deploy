package deployment

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"time"

	deployconfig "github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/transport"
)

//go:embed retention.sh
var retentionScript string

type retentionPolicy struct {
	Enabled    bool  `json:"enabled"`
	Keep       int   `json:"keep"`
	AgeSeconds int64 `json:"age_seconds"`
}

func retentionPolicyFor(r deployconfig.RetentionConfig) retentionPolicy {
	d, _ := time.ParseDuration(r.MaxAge)
	return retentionPolicy{Enabled: r.KeepLast > 0 || d > 0, Keep: r.KeepLast, AgeSeconds: int64(math.Ceil(d.Seconds()))}
}

func PruneHistory(n deployconfig.NormalizedConfig, preview bool) error {
	config := struct {
		Mode        deployconfig.DeploymentMode `json:"mode"`
		Database    string                      `json:"database"`
		Environment string                      `json:"environment"`
		Releases    retentionPolicy             `json:"releases"`
		Backups     retentionPolicy             `json:"backups"`
		Preview     bool                        `json:"preview"`
		UID         int                         `json:"container_uid"`
	}{Mode: n.Mode, Environment: n.Env.Name, Releases: retentionPolicyFor(n.Env.Storage.Releases), Backups: retentionPolicyFor(n.Env.Database.Retention), Preview: preview, UID: n.Env.Quadlet.ContainerUID}
	if n.Env.Database.Driver == "sqlite" {
		_, config.Database, _ = deployconfig.DatabasePaths(n.Env)
	}
	body, err := json.Marshal(config)
	if err != nil {
		return err
	}
	script := remoteOperationGuard(n.Env) + "\nconfiguration=" + transport.ShellQuote(string(body)) + "\n" + retentionScript
	// A preview performs a read under coordination locks; it never deletes history.
	out, err := transport.RunSSHOutputRead(n.Env, script)
	if out != "" {
		fmt.Print(out)
	}
	return err
}
