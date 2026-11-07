package source

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup/helper"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

func sourceInspectionTree(t *testing.T, home string) map[string]string {
	t.Helper()
	result := map[string]string{}
	testutil.Must(t, filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		result[path] = entry.Type().String()
		if entry.Type().IsRegular() {
			result[path], err = safefs.FileChecksum(path)
		}
		return err
	}))
	return result
}

func TestInspectSourceEnrollmentNeverEnrollsOrRepairs(t *testing.T) {
	home := t.TempDir()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	testutil.Must(t, err)
	key, err := ssh.NewPublicKey(public)
	testutil.Must(t, err)
	r := SourceEnrollmentRequest{Schema: 1, Operation: "enroll", WorkerID: testutil.ID(t, "w"), PublicKey: string(ssh.MarshalAuthorizedKey(key))}
	enrolled, err := enrollSourceWorker(home, r)
	testutil.Must(t, err)
	d, err := safefs.Open(filepath.Join(home, ".local/lib/deploy/backup-source"))
	testutil.Must(t, err)
	var artifact helper.HelperArtifactManifest
	testutil.Must(t, d.ReadJSON("artifact.json", &artifact))
	d.Close()
	r.Operation, r.SourceID, r.HelperSHA256 = "inspect", enrolled.SourceID, artifact.Current
	original := sourceInspectionTree(t, home)
	for range 2 {
		got, err := inspectSourceEnrollment(home, r)
		if err != nil || got != enrolled {
			t.Fatal(got, err)
		}
	}
	if !reflect.DeepEqual(original, sourceInspectionTree(t, home)) {
		t.Fatal("inspection changed source state")
	}
	for _, field := range []string{"source", "worker", "key", "helper", "operation", "missing-identity", "missing-lock", "missing-helper", "fifo-helper", "empty-home"} {
		t.Run(field, func(t *testing.T) {
			next, target := r, home
			restore := func() {}
			switch field {
			case "source":
				next.SourceID = testutil.ID(t, "s")
			case "worker":
				next.WorkerID = testutil.ID(t, "w")
			case "key":
				next.PublicKey = "restrict " + next.PublicKey
			case "helper":
				next.HelperSHA256 = strings.Repeat("a", 64)
			case "operation":
				next.Operation = "enroll"
			case "empty-home":
				target = t.TempDir()
			default:
				path := filepath.Join(home, ".local/state/deploy/backup-source/identity.json")
				if field == "missing-lock" {
					path = filepath.Join(home, ".local/state/deploy/backup-source/policy.lock")
				}
				if field == "missing-helper" || field == "fifo-helper" {
					path = filepath.Join(home, ".local/lib/deploy/backup-source/deploy")
				}
				testutil.Must(t, os.Rename(path, path+".retained"))
				if field == "fifo-helper" {
					testutil.Must(t, unix.Mkfifo(path, 0600))
				}
				restore = func() {
					if field == "fifo-helper" {
						testutil.Must(t, os.Remove(path))
					}
					testutil.Must(t, os.Rename(path+".retained", path))
				}
			}
			before := sourceInspectionTree(t, target)
			if _, err := inspectSourceEnrollment(target, next); err == nil {
				t.Fatal("changed source admitted")
			}
			if !reflect.DeepEqual(before, sourceInspectionTree(t, target)) {
				t.Fatal("inspection repaired or enrolled a source")
			}
			restore()
		})
	}
	if !reflect.DeepEqual(original, sourceInspectionTree(t, home)) {
		t.Fatal("inspection changed retained history")
	}
	r.Operation = "revoke"
	_, err = enrollSourceWorker(home, r)
	testutil.Must(t, err)
	r.Operation = "inspect"
	before := sourceInspectionTree(t, home)
	if _, err = inspectSourceEnrollment(home, r); err == nil {
		t.Fatal("revoked source authorization accepted")
	}
	if !reflect.DeepEqual(before, sourceInspectionTree(t, home)) {
		t.Fatal("inspection restored revoked access")
	}
}
