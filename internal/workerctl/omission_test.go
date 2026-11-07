package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/config"
)

type omissionInspector struct {
	countingInspector
	status   TargetStatus
	statuses int
}

func (i *omissionInspector) Status(context.Context, config.ServerConfig, string, ResolvedPaths) (TargetStatus, error) {
	i.statuses++
	return i.status, nil
}

func TestOmittedWorkerPerformsOnlyReadOnlyReview(t *testing.T) {
	for _, kind := range []string{"backup-worker", "notification-worker"} {
		for _, op := range []string{"apply", "resume", "plan", "status", "uninstall"} {
			t.Run(kind+"/"+op, func(t *testing.T) {
				root := t.TempDir()
				file := filepath.Join(root, "server.yaml")
				if err := os.WriteFile(file, []byte("version: 3\nservers:\n  manager: {host: example.test, user: deploy}\n"), 0600); err != nil {
					t.Fatal(err)
				}
				before := planTree(t, root)
				i := &omissionInspector{status: TargetStatus{LoadState: "loaded", ActiveState: "active", Paths: []PathObservation{{Name: "manifest", Exists: true, Safe: true}, {Name: "state", Exists: true, Safe: true}}}}
				controller := &planningController{}
				var output bytes.Buffer
				cli := CLI{Out: &output, Inspector: i, Controller: controller}
				if err := cli.Run(context.Background(), kind, []string{op, "manager"}, Options{Inventory: file, JSON: true, NonInteractive: true, Catalog: "/missing-catalog"}); err != nil {
					t.Fatal(err)
				}
				var result Plan
				if err := json.Unmarshal(output.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if !result.Omitted || result.Code != "removal_review" || result.State != "needs_action" || result.Artifacts != nil || len(result.Actions) != 0 || i.Calls != 1 || i.statuses != 1 || controller.calls != 0 {
					t.Fatal("omission mutated or implied ownership", result, i, controller)
				}
				if !reflect.DeepEqual(before, planTree(t, root)) {
					t.Fatal("omission wrote inventory or credentials")
				}
			})
		}
	}
}

func TestOmittedWorkerAbsenceAndUnknownManagerDiffer(t *testing.T) {
	for _, kind := range []string{"absent", "unknown", "unsafe", "missing-manifest"} {
		t.Run(kind, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "server.yaml")
			if err := os.WriteFile(file, []byte("version: 3\nservers:\n  manager: {host: example.test, user: deploy}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			s := TargetStatus{LoadState: "not-found", ActiveState: "inactive", Paths: []PathObservation{{Name: "manifest", Safe: true}, {Name: "state", Safe: true}}}
			want := "not_requested"
			switch kind {
			case "unknown":
				s.LoadState, s.ActiveState = "unknown", "unknown"
				want = "presence_unverified"
			case "unsafe":
				s.Paths[1].Exists = true
				s.Paths[1].Safe = false
				want = "unsafe_path"
			case "missing-manifest":
				s.Paths[1].Exists = true
				want = "missing_manifest"
			}
			i := &omissionInspector{status: s}
			var output bytes.Buffer
			if err := (CLI{Out: &output, Inspector: i}).Run(context.Background(), "notification-worker", []string{"manager"}, Options{Inventory: file, JSON: true}); err != nil {
				t.Fatal(err)
			}
			var result Plan
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Code != want {
				t.Fatal("absence or recovery misreported", result)
			}
		})
	}
}
