package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dryaf/deploy/internal/safefs"
)

// A fresh installation owns no direct sender. Do not stop or adopt a foreign
// service, even when its name matches an old deploy unit. Require absence both
// from the managed paths and from systemd (including aliases/generated units).
func (u *backupCutoverManager) requireNoDirectSenders(ctx context.Context) error {
	root, err := safefs.Open(u.home)
	if err != nil {
		return err
	}
	defer root.Close()
	run := u.run
	if run == nil {
		run = cutoverCommand
	}
	names := []string{"deploy-backup-notifier.service", "deploy-backup-notifier.timer", "deploy-backup-notifier.path", "deploy-backup-test-telegram.service", "deploy-backup-test-signal.service", "deploy-backup-signal.service", "deploy-backup-legacy.service", "deploy-backup-legacy.timer", "deploy-backup-legacy.path"}
	for _, name := range names {
		paths := []string{".config/systemd/user/" + name, ".config/systemd/user/" + name + ".d", filepath.Join(u.unitDirectory(), name), filepath.Join(u.unitDirectory(), name+".d")}
		if strings.HasSuffix(name, ".service") {
			container := strings.TrimSuffix(name, ".service") + ".container"
			paths = append(paths, ".config/containers/systemd/"+container, ".config/containers/systemd/"+container+".d", filepath.Join(u.quadletDirectory(), container), filepath.Join(u.quadletDirectory(), container+".d"))
		}
		for _, path := range paths {
			f, e := openBackupOwnedFile(root, path, os.O_RDONLY, 0)
			if e == nil {
				f.Close()
				return fmt.Errorf("fresh shared reporting found a direct-sender artifact; review required")
			}
			if !os.IsNotExist(e) {
				return e
			}
		}
		properties := "LoadState,ActiveState,FragmentPath,SourcePath,DropInPaths,Names"
		service := strings.HasSuffix(name, ".service")
		if service {
			properties += ",MainPID,ControlPID"
		}
		body, _ := run(ctx, "systemctl", "--user", "--no-pager", "show", name, "--property="+properties)
		values := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if _, duplicate := values[k]; !ok || duplicate {
				return fmt.Errorf("invalid direct-sender absence inspection")
			}
			values[k] = v
		}
		// systemctl may return nonzero for a proven not-found unit. No other
		// nonzero result, incomplete property set or surviving PID is accepted.
		if len(values) != len(strings.Split(properties, ",")) || values["LoadState"] != "not-found" || values["ActiveState"] != "inactive" || values["FragmentPath"] != "" || values["SourcePath"] != "" || values["DropInPaths"] != "" || values["Names"] != "" && values["Names"] != name || service && (values["MainPID"] != "0" || values["ControlPID"] != "0") {
			return fmt.Errorf("direct-sender absence not proven; review service ownership")
		}
		if strings.HasSuffix(name, ".service") {
			body, err = run(ctx, "podman", "ps", "--filter", "name=^systemd-"+strings.TrimSuffix(name, ".service")+"$", "--format", "{{.Names}}")
			if err != nil || strings.TrimSpace(string(body)) != "" {
				return fmt.Errorf("direct-sender container absence not proven")
			}
		}
	}
	return nil
}
