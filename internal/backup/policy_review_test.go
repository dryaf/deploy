package backup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"github.com/dryaf/deploy/internal/backup/source"
	"golang.org/x/crypto/ssh"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func policyReviewTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	testutil.Must(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files[path] = info.Mode().String()
		if info.Mode().IsRegular() {
			raw, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			files[path] += fmtDigest(raw)
		}
		return nil
	}))
	return files
}

func TestLegacyBackupPolicyReviewWithoutAdoption(t *testing.T) {
	for _, damage := range []string{"", "runtime", "helper", "state", "journal", "shared", "pending", "busy", "wal", "source-key", "source-receipt", "source-trust"} {
		t.Run(damage, func(t *testing.T) {
			home, state, j := setupObservationFixture(t)
			completePolicyReviewSources(t, home, &j)
			// Close the fixture writer before taking a byte-level snapshot; SQLite
			// reader marks in a live shared-memory WAL index are not policy writes.
			testutil.Must(t, state.Close())
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			j.Declaration = nil
			if damage == "shared" {
				j.Producer = &ProducerReservation{}
			}
			if damage == "pending" {
				j.Updating = true
			}
			testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
			switch damage {
			case "source-key", "source-trust":
				var c WorkerInstallation
				testutil.Must(t, root.ReadJSON(workerRuntimeConfig, &c))
				path := c.Sources[0].KeyFile
				if damage == "source-trust" {
					path = c.Sources[0].KnownHosts
				}
				testutil.Must(t, os.Remove(path))
			case "source-receipt":
				j.SourceEnrollments = nil
				testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
			case "busy":
				lease, e := root.Lock(".local/state/deploy/backup-worker/state/schema.lock", false, false)
				testutil.Must(t, e)
				defer lease.Close()
			case "wal":
				testutil.Must(t, os.WriteFile(filepath.Join(home, ".local/state/deploy/backup-worker/state/worker.db-wal"), []byte("unsettled WAL must never be ignored"), 0600))
			case "runtime":
				testutil.Must(t, os.WriteFile(filepath.Join(home, ".config/deploy/backup-worker/runtime.json"), []byte("{}"), 0600))
			case "helper":
				testutil.Must(t, os.WriteFile(filepath.Join(home, ".local/lib/deploy/backup-worker/deploy"), []byte("changed helper"), 0700))
			case "state":
				testutil.Must(t, os.Rename(filepath.Join(home, ".local/state/deploy/backup-worker/state"), filepath.Join(home, "retained-state")))
			case "journal":
				testutil.Must(t, os.Remove(filepath.Join(home, backupSetupFile)))
			}
			before := policyReviewTree(t, home)
			got, err := InspectSetupPolicyReview(context.Background(), home)
			if damage == "" {
				if err != nil || got.Installation.WorkerID != j.WorkerID || got.Installation.ArchiveID != j.ArchiveID || got.Desired != nil || got.Paths.State == "" {
					t.Fatal("lost legacy authority", err)
				}
				status, e := InspectSetup(context.Background(), home)
				if e != nil || status.Stage != "adoption_required" {
					t.Fatal("review silently adopted legacy installation", status, e)
				}
			} else if err == nil {
				t.Fatal("invalid authority exported")
			}
			if after := policyReviewTree(t, home); !reflect.DeepEqual(before, after) {
				for path, hash := range after {
					if before[path] != hash {
						t.Log("changed:", path)
					}
				}
				for path := range before {
					if _, ok := after[path]; !ok {
						t.Log("removed:", path)
					}
				}
				t.Fatal("read-only review changed retained files")
			}
		})
	}
}

// Real source enrollment/key material is needed for legacy policy inspection;
// the lighter status fixture intentionally has no remote-key setup.
func completePolicyReviewSources(t *testing.T, home string, j *WorkerSetupJournal) {
	t.Helper()
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	paths, err := declarationPaths(home, j.Declaration)
	testutil.Must(t, err)
	testutil.Must(t, root.Mkdir(paths.OwnedPath(paths.Keys())))
	var c WorkerInstallation
	testutil.Must(t, root.ReadJSON(workerRuntimeConfig, &c))
	j.SourceEnrollments = map[string]source.SourceEnrollmentResult{}
	for i, s := range c.Sources {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		testutil.Must(t, err)
		block, err := ssh.MarshalPrivateKey(private, "fixture")
		testutil.Must(t, err)
		key, err := ssh.NewPublicKey(public)
		testutil.Must(t, err)
		s.KeyFile, s.KnownHosts = filepath.Join(paths.Keys(), s.Alias+".key"), filepath.Join(paths.Keys(), s.Alias+".known_hosts")
		testutil.Must(t, os.WriteFile(s.KeyFile, pem.EncodeToMemory(block), 0600))
		testutil.Must(t, os.WriteFile(s.KnownHosts, append([]byte(s.Host+" "), ssh.MarshalAuthorizedKey(key)...), 0600))
		j.SourceEnrollments[s.Alias] = source.SourceEnrollmentResult{Schema: 1, WorkerID: j.WorkerID, SourceID: s.SourceID, Fingerprint: ssh.FingerprintSHA256(key)}
		c.Sources[i] = s
	}
	body, err := json.Marshal(c)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(filepath.Join(home, workerRuntimeConfig), body, 0600))
	j.OwnedFiles[workerRuntimeConfig] = sharedSetupDigest(body)
}
