//go:build linux

package source

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"golang.org/x/crypto/ssh"
)

func TestBackupSourceEnrollmentRestrictionsAndRevocation(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	unrelated := string(ssh.MarshalAuthorizedKey(key))
	unrelated = strings.TrimSpace(unrelated) + " administrator\n"
	if err = os.WriteFile(filepath.Join(home, ".ssh/authorized_keys"), []byte(unrelated), 0600); err != nil {
		t.Fatal(err)
	}
	request := SourceEnrollmentRequest{Schema: 1, Operation: "enroll", WorkerID: testutil.ID(t, "w"), PublicKey: string(ssh.MarshalAuthorizedKey(key))}
	first, err := enrollSourceWorker(home, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := enrollSourceWorker(home, request)
	if err != nil || first != second {
		t.Fatal(first, second, err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".ssh/authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte(unrelated)) {
		t.Fatal("administrator key changed")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatal(string(data))
	}
	_, comment, options, _, err := ssh.ParseAuthorizedKey([]byte(lines[1]))
	if err != nil || comment != "deploy-backup:"+request.WorkerID {
		t.Fatal(err, comment)
	}
	joined := strings.Join(options, " ")
	if !strings.Contains(joined, "restrict") || !strings.Contains(joined, "source-serve --worker-id "+request.WorkerID+" --key-id "+ssh.FingerprintSHA256(key)) {
		t.Fatal("missing forced key restrictions", joined)
	}
	request.Operation = "revoke"
	if _, err = enrollSourceWorker(home, request); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(home, ".ssh/authorized_keys"))
	if string(data) != unrelated {
		t.Fatal("unrelated key changed during revoke")
	}
	if _, _, err = loadSourceIdentity(home, request.WorkerID); err == nil {
		t.Fatal("revoked worker remained authorized")
	}
}

func TestBackupSourceGenerationIsAuthoritative(t *testing.T) {
	metadata := quadlet.QuadletMetadata{Schema: 1, ManagedBy: "deploy", Role: "application", App: "foodlog", Environment: "prod", Service: "foodlog-prod", Mode: config.ModeImage, TargetDir: "/srv/foodlog", Backup: &quadlet.QuadletBackupMetadata{Enabled: true, Driver: "sqlite", Source: "data/app.db"}}
	attempt := protocol.SnapshotAttempt{Protocol: 1, Operation: "snapshot", SourceID: testutil.ID(t, "s"), Service: metadata.Service, Generation: "unenrolled", BindingHash: protocol.MetadataBindingHash(metadata)}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local/state/deploy/backup-source/generations"), 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := safefs.Open(filepath.Join(home, ".local/state/deploy/backup-source"))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	worker := testutil.ID(t, "w")
	identity := SourceIdentity{1, attempt.SourceID, map[string]SourceAuthorization{worker: {Enabled: true, Fingerprint: "test"}}, map[string]string{}}
	if err = dir.WriteJSON("identity.json", identity, false); err != nil {
		t.Fatal(err)
	}
	lock, err := dir.Lock("policy.lock", true, true)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	generation, err := SourceGenerationForExport(dir, identity, worker, metadata, attempt)
	if err != nil {
		t.Fatal(err)
	}
	again, err := SourceGenerationForExport(dir, identity, worker, metadata, attempt)
	if err != nil || again != generation {
		t.Fatal("generation changed on repeated enrollment", err)
	}
	attempt.Generation = generation
	if err = os.Remove(filepath.Join(dir.File().Name(), "generations", safefs.LockName(attempt.Service)+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err = SourceGenerationForExport(dir, identity, worker, metadata, attempt); err == nil {
		t.Fatal("lost generation silently recreated")
	}
	attempt.Generation = "unenrolled"
	if _, err = SourceGenerationForExport(dir, identity, worker, metadata, attempt); err == nil {
		t.Fatal("lost ledger generation silently reenrolled")
	}
}

func TestBackupSourceRotationRejectsStaleAuthority(t *testing.T) {
	home := t.TempDir()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := ssh.NewPublicKey(public)
	request := SourceEnrollmentRequest{Schema: 1, Operation: "enroll", WorkerID: testutil.ID(t, "w"), PublicKey: string(ssh.MarshalAuthorizedKey(first))}
	enrolled, err := enrollSourceWorker(home, request)
	if err != nil {
		t.Fatal(err)
	}
	public, _, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	next, _ := ssh.NewPublicKey(public)
	request.Operation = "rotate"
	request.SourceID = enrolled.SourceID
	request.PublicKey = string(ssh.MarshalAuthorizedKey(next))
	request.Previous = "wrong"
	if _, err = enrollSourceWorker(home, request); err == nil {
		t.Fatal("rotation accepted stale previous fingerprint")
	}
	request.Previous = enrolled.Fingerprint
	rotated, err := enrollSourceWorker(home, request)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := enrollSourceWorker(home, request)
	if err != nil || retried != rotated {
		t.Fatal("rotation is not retryable", err)
	}
	body, err := os.ReadFile(filepath.Join(home, ".ssh/authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), enrolled.Fingerprint) || !strings.Contains(string(body), rotated.Fingerprint) {
		t.Fatal("old forced key remained authorized")
	}
}
