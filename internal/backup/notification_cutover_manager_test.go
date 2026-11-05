//go:build linux

package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupCutoverManagerRejectsUnprovenStop(t *testing.T) {
	name := "deploy-backup-notifier.service"
	source := "/home/fixture/.config/containers/systemd/deploy-backup-notifier.container"
	baseline := map[string]string{"LoadState": "loaded", "ActiveState": "inactive", "FragmentPath": fmt.Sprintf("/run/user/%d/systemd/generator/%s", os.Getuid(), name), "SourcePath": source, "DropInPaths": "", "Names": name, "MainPID": "0", "ControlPID": "0", "NeedDaemonReload": "no", "UnitFileState": "generated", "KillMode": "control-group", "SendSIGKILL": "yes"}
	properties := baseline
	u := backupCutoverManager{run: func(context.Context, string, ...string) ([]byte, error) {
		var out strings.Builder
		for k, v := range properties {
			fmt.Fprintf(&out, "%s=%s\n", k, v)
		}
		return []byte(out.String()), nil
	}}
	if err := u.inspect(context.Background(), name, source, true, false); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"ActiveState": "active", "FragmentPath": "/foreign/service", "SourcePath": "/foreign/container", "DropInPaths": "/foreign/dropin.conf", "Names": name + " alias.service", "MainPID": "42", "ControlPID": "43", "NeedDaemonReload": "yes", "KillMode": "process", "SendSIGKILL": "no"} {
		t.Run(k, func(t *testing.T) {
			properties = map[string]string{}
			for key, value := range baseline {
				properties[key] = value
			}
			properties[k] = v
			if err := u.inspect(context.Background(), name, source, true, false); err == nil {
				t.Fatal("unproven sender accepted")
			}
		})
	}
}
func TestBackupCutoverReportForeignFiles(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config/systemd/user")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "deploy-backup-report.service")
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	u := backupCutoverManager{home: home}
	if err := u.InstallReports(context.Background(), map[string]string{"deploy-backup-report.service": "ours"}); err == nil {
		t.Fatal("overwrote foreign report service")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "foreign" {
		t.Fatal("foreign file changed", err)
	}
}

func TestBackupCutoverRejectsForeignReportUnitNames(t *testing.T) {
	name := "deploy-backup-report.service"
	output := "LoadState=not-found\nActiveState=inactive\nFragmentPath=\nDropInPaths=\nNames=\n"
	u := backupCutoverManager{home: "/home/fixture", run: func(context.Context, string, ...string) ([]byte, error) { return []byte(output), nil }}
	if err := u.reportSlot(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		"LoadState=loaded\nActiveState=inactive\nFragmentPath=/etc/systemd/user/foreign.service\nDropInPaths=\nNames=" + name + "\n",
		"LoadState=not-found\nActiveState=inactive\nFragmentPath=\nDropInPaths=/foreign.conf\nNames=\n",
		"LoadState=not-found\nActiveState=inactive\nFragmentPath=\nDropInPaths=\nNames=alias.service\n",
	} {
		output = body
		if err := u.reportSlot(context.Background(), name); err == nil {
			t.Fatal("foreign report name accepted")
		}
	}
}
