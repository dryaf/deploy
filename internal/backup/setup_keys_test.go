package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDeclarativeSourceKeyReservationSurvivesRetryAndRejectsLoss(t *testing.T) {
	for _, scenario := range []string{"missing", "changed", "hardlink"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			for _, dir := range []string{".config/deploy/backup-worker/ssh", ".local/state/deploy/backup-worker"} {
				testutil.Must(t, root.Mkdir(dir))
			}
			keys, err := root.Sub(".config/deploy/backup-worker/ssh")
			testutil.Must(t, err)
			defer keys.Close()
			state, err := root.Sub(".local/state/deploy/backup-worker")
			testutil.Must(t, err)
			defer state.Close()
			j := WorkerSetupJournal{Schema: 1, WorkerID: testutil.ID(t, "w"), Declaration: &SetupDeclaration{Version: 1}}
			testutil.Must(t, state.WriteJSON("setup.json", j, false))
			public, err := setupSourceKey(home, keys, state, &j, "source")
			testutil.Must(t, err)
			var retained WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &retained))
			if retained.SourceKeys["source"] == "" {
				t.Fatal("key exposed before durable reservation")
			}
			again, err := setupSourceKey(home, keys, state, &retained, "source")
			if err != nil || again != public {
				t.Fatal("retry replaced source identity", err)
			}
			completed := retained
			completed.Declaration = &SetupDeclaration{Version: 1, Sources: map[string]string{"source": "fixture-helper-pin"}}
			completed.SourceKeys = map[string]string{"source": retained.SourceKeys["source"]}
			completed.SourceEnrollments = map[string]source.SourceEnrollmentResult{"source": {Fingerprint: retained.SourceKeys["source"]}}
			testutil.Must(t, verifySetupSourceKeys(root, completed))
			keyPath := filepath.Join(home, ".config/deploy/backup-worker/ssh/source.key")
			switch scenario {
			case "missing":
				testutil.Must(t, os.Remove(keyPath))
			case "changed":
				_, err = setupSourceKey(home, keys, state, &retained, "other")
				testutil.Must(t, err)
				raw, err := os.ReadFile(filepath.Join(home, ".config/deploy/backup-worker/ssh/other.key"))
				testutil.Must(t, err)
				testutil.Must(t, os.WriteFile(keyPath, raw, 0600))
			case "hardlink":
				testutil.Must(t, os.Link(keyPath, filepath.Join(home, "extra-key-link")))
			}
			if _, err = setupSourceKey(home, keys, state, &retained, "source"); err == nil {
				t.Fatal("lost or changed source key admitted")
			}
			if err = verifySetupSourceKeys(root, completed); err == nil {
				t.Fatal("completed retry or status admitted lost/changed key")
			}
			if scenario == "missing" {
				if _, err = os.Stat(keyPath); !os.IsNotExist(err) {
					t.Fatal("lost key regenerated", err)
				}
			}
			var after WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &after))
			if after.SourceKeys["source"] != j.SourceKeys["source"] {
				t.Fatal("reservation changed after refusal")
			}
		})
	}
}
