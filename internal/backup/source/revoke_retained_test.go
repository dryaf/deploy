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

func retainedRevocationFixture(t *testing.T) (string, SourceEnrollmentRequest) {
	t.Helper()
	home := t.TempDir()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	testutil.Must(t, err)
	key, err := ssh.NewPublicKey(pub)
	testutil.Must(t, err)
	r := SourceEnrollmentRequest{Schema: 1, Operation: "enroll", WorkerID: testutil.ID(t, "w"), PublicKey: string(ssh.MarshalAuthorizedKey(key))}
	e, err := enrollSourceWorker(home, r)
	testutil.Must(t, err)
	d, err := safefs.Open(filepath.Join(home, ".local/lib/deploy/backup-source"))
	testutil.Must(t, err)
	var manifest helper.HelperArtifactManifest
	testutil.Must(t, d.ReadJSON("artifact.json", &manifest))
	d.Close()
	return home, SourceEnrollmentRequest{Schema: 1, Operation: "revoke-retained", SourceID: e.SourceID, WorkerID: e.WorkerID, Previous: e.Fingerprint, HelperSHA256: manifest.Current, Reason: "database moved to another backup policy"}
}

func TestRetainedSourceRevocationPreservesHistoryAndRetries(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "after-policy-disable"}[interrupted], func(t *testing.T) {
			home, r := retainedRevocationFixture(t)
			d, err := safefs.Open(filepath.Join(home, ".local/state/deploy/backup-source"))
			testutil.Must(t, err)
			defer d.Close()
			var identity SourceIdentity
			testutil.Must(t, d.ReadJSON("identity.json", &identity))
			if interrupted {
				known := identity.Workers[r.WorkerID]
				known.Enabled, known.RetirementReason = false, r.Reason
				identity.Workers[r.WorkerID] = known
				testutil.Must(t, d.WriteJSON("identity.json", identity, true))
			}
			keys := filepath.Join(home, ".ssh/authorized_keys")
			old, err := os.ReadFile(keys)
			testutil.Must(t, err)
			foreign := "# unrelated operator access\n\n"
			testutil.Must(t, os.WriteFile(keys, append([]byte(foreign), old...), 0600))
			before := sourceInspectionTree(t, home)
			for range 2 {
				got, err := revokeRetainedSource(home, r)
				if err != nil || got != (SourceEnrollmentResult{Schema: 1, SourceID: r.SourceID, WorkerID: r.WorkerID}) {
					t.Fatal(got, err)
				}
			}
			body, err := os.ReadFile(keys)
			testutil.Must(t, err)
			if string(body) != foreign {
				t.Fatalf("unrelated keys changed: %q", body)
			}
			testutil.Must(t, d.ReadJSON("identity.json", &identity))
			known := identity.Workers[r.WorkerID]
			if known.Enabled || known.Fingerprint != r.Previous || known.RetirementReason != r.Reason {
				t.Fatal("retirement lost identity or disposition", known)
			}
			after := sourceInspectionTree(t, home)
			for _, path := range []string{keys, filepath.Join(home, ".local/state/deploy/backup-source/identity.json")} {
				delete(before, path)
				delete(after, path)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("retirement changed helper, generation or rate history")
			}
			pub, _, err := ed25519.GenerateKey(rand.Reader)
			testutil.Must(t, err)
			key, err := ssh.NewPublicKey(pub)
			testutil.Must(t, err)
			tree := sourceInspectionTree(t, home)
			for _, operation := range []string{"enroll", "rotate"} {
				_, err = enrollSourceWorker(home, SourceEnrollmentRequest{Schema: 1, Operation: operation, SourceID: r.SourceID, WorkerID: r.WorkerID, Previous: r.Previous, PublicKey: string(ssh.MarshalAuthorizedKey(key))})
				if err == nil || !reflect.DeepEqual(tree, sourceInspectionTree(t, home)) {
					t.Fatal("ordinary enrollment resurrected retired access or changed its helper", err)
				}
			}
			r.Reason = "different intent"
			if _, err = revokeRetainedSource(home, r); err == nil {
				t.Fatal("changed retry disposition accepted")
			}
		})
	}
}

func TestRetainedSourceRevocationRejectsDriftBeforeEffects(t *testing.T) {
	for _, damage := range []string{"source", "worker", "fingerprint", "helper", "reason", "empty-home", "missing-identity", "missing-lock", "missing-helper", "fifo-helper", "missing-keys", "fifo-keys", "foreign-key", "missing-owned-key", "unsafe-keys"} {
		t.Run(damage, func(t *testing.T) {
			home, r := retainedRevocationFixture(t)
			switch damage {
			case "source":
				r.SourceID = testutil.ID(t, "s")
			case "worker":
				r.WorkerID = testutil.ID(t, "w")
			case "fingerprint":
				r.Previous = "different-key"
			case "helper":
				r.HelperSHA256 = strings.Repeat("a", 64)
			case "reason":
				r.Reason = ""
			case "empty-home":
				home = t.TempDir()
			case "foreign-key", "missing-owned-key", "unsafe-keys":
				path := filepath.Join(home, ".ssh/authorized_keys")
				if damage == "unsafe-keys" {
					testutil.Must(t, os.Chmod(path, 0666))
				} else {
					body := "# unrelated\n"
					if damage == "foreign-key" {
						body += "# deploy-backup:" + r.WorkerID + "\n"
					}
					testutil.Must(t, os.WriteFile(path, []byte(body), 0600))
				}
			default:
				name := map[string]string{"missing-identity": ".local/state/deploy/backup-source/identity.json", "missing-lock": ".local/state/deploy/backup-source/policy.lock", "missing-helper": ".local/lib/deploy/backup-source/deploy", "fifo-helper": ".local/lib/deploy/backup-source/deploy", "missing-keys": ".ssh/authorized_keys", "fifo-keys": ".ssh/authorized_keys"}[damage]
				path := filepath.Join(home, name)
				testutil.Must(t, os.Remove(path))
				if strings.HasPrefix(damage, "fifo-") {
					testutil.Must(t, unix.Mkfifo(path, 0600))
				}
			}
			before := sourceInspectionTree(t, home)
			if _, err := revokeRetainedSource(home, r); err == nil {
				t.Fatal("drift accepted")
			}
			if !reflect.DeepEqual(before, sourceInspectionTree(t, home)) {
				t.Fatal("rejected retirement changed source state")
			}
		})
	}
}
