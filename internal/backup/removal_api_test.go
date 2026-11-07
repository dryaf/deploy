package backup

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestDeclarativeRemovalRetainsIdentityAcrossLostReplies(t *testing.T) {
	w, _ := safefs.NewID("w")
	a, _ := safefs.NewID("a")
	s, _ := safefs.NewID("s")
	c := WorkerInstallation{WorkerID: w, ArchiveID: a, Sources: []source.WorkerSource{{Alias: "source", Host: "fixture", User: "app", Port: 22, SourceID: s}}, Policy: config.BackupWorkerConfig{Storage: config.WorkerStorage{Path: "/archive"}}}
	j := workerLifecycleJournal{Schema: 1, Operation: "uninstall", Stage: "prepared", Installation: c}
	hash := strings.Repeat("a", 64)
	var calls []string
	call := func(r workerLifecycleRequest) (workerLifecycleJournal, error) {
		calls = append(calls, r.Operation)
		if r.DesiredHash != hash {
			t.Fatal("lost desired policy pin")
		}
		if r.Operation == "uninstall-finish" {
			if len(r.Results) != 1 || r.Results[0].SourceID != s || r.Results[0].WorkerID != w || r.Results[0].Fingerprint != "" {
				t.Fatal("lost revocation acknowledgements")
			}
			j.Stage = "complete"
			return j, errors.New("lost completion response")
		}
		return j, nil
	}
	revoke := func(admin config.ServerConfig, r source.SourceEnrollmentRequest) (source.SourceEnrollmentResult, error) {
		calls = append(calls, "revoke")
		if r.Operation != "revoke" || r.WorkerID != w || r.SourceID != s || admin.Host != "fixture" {
			t.Fatal("wrong revocation scope")
		}
		return source.SourceEnrollmentResult{Schema: 1, WorkerID: w, SourceID: s}, nil
	}
	admins := map[string]config.ServerConfig{"source": {Host: "fixture", User: "app"}}
	if _, err := uninstallWorkerUsing(context.Background(), admins, hash, call, revoke); err == nil {
		t.Fatal("lost response hidden")
	}
	if !reflect.DeepEqual(calls, []string{"uninstall-prepare", "revoke", "uninstall-finish"}) {
		t.Fatal(calls)
	}
	calls = nil
	r, err := uninstallWorkerUsing(context.Background(), admins, hash, call, revoke)
	if err != nil || r.WorkerID != w || r.ArchiveID != a || r.ArchivePath != "/archive" || !reflect.DeepEqual(calls, []string{"uninstall-prepare"}) {
		t.Fatal("completed retry repeated removal", r, calls, err)
	}
	j.Stage = "prepared"
	calls = nil
	admins["source"] = config.ServerConfig{Host: "fixture", User: "app", SSHPort: 2222}
	if _, err = uninstallWorkerUsing(context.Background(), admins, hash, call, revoke); err == nil {
		t.Fatal("changed SSH endpoint accepted")
	}
	if !reflect.DeepEqual(calls, []string{"uninstall-prepare"}) {
		t.Fatal("revoked before endpoint validation", calls)
	}
	j.Installation.Policy.Notifications.Mode = "shared"
	admins["source"] = config.ServerConfig{Host: "fixture", User: "app"}
	calls = nil
	if _, err = uninstallWorkerUsing(context.Background(), admins, hash, call, revoke); err == nil || !strings.Contains(err.Error(), "retirement") {
		t.Fatal("shared producer was removed before retirement", err)
	}
	if !reflect.DeepEqual(calls, []string{"uninstall-prepare"}) {
		t.Fatal("revoked shared source before retirement", calls)
	}
}
