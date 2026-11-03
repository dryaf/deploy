//go:build linux

package backup

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func archiveFixture(t *testing.T) (*backupArchive, *backupState, protocol.SnapshotAttempt, protocol.SnapshotHeader, []byte) {
	t.Helper()
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	if err := os.Mkdir(archiveDir, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := enrollBackupArchive(archiveDir, testutil.ID(t, "a"), testutil.ID(t, "w"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	state, err := openBackupState(filepath.Join(dir, "state"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	control := ArchiveControl{Schema: 1, ArchiveID: a.identity.ID, WriterID: a.identity.WriterID, Epoch: 1, Revision: 1, ConfigHash: strings.Repeat("a", 64), Instances: map[string]ExpectedInstance{}, RetiredSources: map[string]string{}, Pins: map[string]string{}}
	if err = state.put("control", control); err != nil {
		t.Fatal(err)
	}
	if err = state.put("mode", "writer"); err != nil {
		t.Fatal(err)
	}
	if err = a.commitControl(state); err != nil {
		t.Fatal(err)
	}
	attempt := protocol.SnapshotAttempt{Protocol: 1, Operation: "snapshot", Nonce: testutil.ID(t, "n"), RunID: testutil.ID(t, "r"), ConfigHash: control.ConfigHash, CaptureSequence: 1, SourceID: testutil.ID(t, "s"), Service: "foodlog-prod", Generation: "unenrolled", RequestedAt: time.Now().UTC()}
	metadata := quadlet.QuadletMetadata{Schema: 1, ManagedBy: "deploy", Role: "application", App: "foodlog", Environment: "prod", Service: attempt.Service, Mode: config.ModeImage, TargetDir: "/srv/foodlog", Backup: &quadlet.QuadletBackupMetadata{Enabled: true, Driver: "sqlite", Source: "data/app.db"}}
	attempt.BindingHash = protocol.MetadataBindingHash(metadata)
	database := filepath.Join(dir, "fixture.db")
	testutil.CreateDatabase(t, database, "archived")
	data, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	header := protocol.SnapshotHeader{Protocol: 1, Role: "source-snapshot", Attempt: attempt, SnapshotID: testutil.ID(t, "b"), Generation: testutil.ID(t, "g"), Metadata: metadata, ReleaseID: "release-1", StartedAt: time.Now().UTC(), Size: int64(len(data)), SHA256: fmtDigest(data), Format: "sqlite-delete-v1", SQLiteVersion: "fixture"}
	header.FinishedAt = header.StartedAt
	if err = state.put("mode", "writer"); err != nil {
		t.Fatal(err)
	}
	if err = a.updateControl(state, func(c *ArchiveControl) error {
		c.Instances[instanceKey(attempt.SourceID, attempt.Service)] = ExpectedInstance{SourceID: attempt.SourceID, Alias: "vps-main", Service: attempt.Service, Generation: attempt.Generation, BindingHash: attempt.BindingHash, CaptureSequence: attempt.CaptureSequence}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a, state, attempt, header, data
}

func fmtDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return strings.ToLower(fmtHex(digest[:]))
}

func fmtHex(data []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(data)*2)
	for i, b := range data {
		out[2*i] = digits[b>>4]
		out[2*i+1] = digits[b&15]
	}
	return string(out)
}

func fixtureValidator(file *os.File) (protocol.SnapshotValidation, error) {
	return validateArchivedFile(context.Background(), file, false)
}

func publishSnapshotFixture(t *testing.T, a *backupArchive, s *backupState, header protocol.SnapshotHeader, data []byte) SnapshotReceipt {
	t.Helper()
	testutil.Must(t, a.updateControl(s, func(c *ArchiveControl) error {
		key := instanceKey(header.Attempt.SourceID, header.Attempt.Service)
		v := c.Instances[key]
		v.CaptureSequence = header.Attempt.CaptureSequence
		c.Instances[key] = v
		return nil
	}))
	r, err := a.receive(s, header.Attempt, "source", 1<<20, func(f *os.File) (protocol.SnapshotHeader, error) {
		_, err := f.Write(data)
		return header, err
	}, fixtureValidator)
	testutil.Must(t, err)
	return r
}

func recordVerificationFixture(t *testing.T, a *backupArchive, s *backupState, r SnapshotReceipt, kind, check string) {
	t.Helper()
	now := time.Now().UTC()
	record := ArchiveVerification{Schema: 1, ID: testutil.ID(t, "n"), ArchiveID: a.identity.ID, BackupID: r.BackupID, SHA256: r.Header.SHA256, StartedAt: now, FinishedAt: now, ErrorKind: kind}
	if kind == "" {
		record.Validation = protocol.SnapshotValidation{Result: "ok", Check: check, CheckedAt: now}
	}
	testutil.Must(t, a.updateControl(s, func(c *ArchiveControl) error { return a.writeVerification(s, c, &record) }))
}

func healthyWorkerFixture(t *testing.T) (*backupArchive, *backupState, WorkerInstallation, SnapshotReceipt) {
	t.Helper()
	a, s, attempt, h, data := archiveFixture(t)
	r, err := a.receive(s, attempt, "source", 1<<20, func(f *os.File) (protocol.SnapshotHeader, error) { _, e := f.Write(data); return h, e }, func(f *os.File) (protocol.SnapshotValidation, error) {
		return validateArchivedFile(context.Background(), f, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = a.updateControl(s, func(control *ArchiveControl) error {
		key := instanceKey(attempt.SourceID, attempt.Service)
		v := control.Instances[key]
		v.Generation = r.Header.Generation
		v.LastSuccess = now
		control.Instances[key] = v
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var control ArchiveControl
	if err = s.get("control", &control); err != nil {
		t.Fatal(err)
	}
	c := WorkerInstallation{Schema: 1, WorkerID: a.identity.WriterID, ArchiveID: a.identity.ID, ConfigHash: control.ConfigHash, Policy: configfixture.WorkerPolicy(t), Qualification: map[string]bool{}}
	for _, gate := range []string{"source_restrictions", "rootless_runtime", "storage_durability", "notification_delivery", "external_monitor", "application_restore"} {
		c.Qualification[gate] = true
	}
	for _, alias := range c.Policy.Sources {
		c.Sources = append(c.Sources, source.WorkerSource{Alias: alias, Host: "localhost", User: "fixture", Port: 22, KeyFile: "/tmp/review.key", KnownHosts: "/tmp/review.known_hosts", SourceID: testutil.ID(t, "s")})
	}
	if err = s.put("run", WorkerRun{ID: testutil.ID(t, "r"), ConfigHash: c.ConfigHash, StartedAt: now.Add(-time.Minute), FinishedAt: now, Complete: true, Captured: 1}); err != nil {
		t.Fatal(err)
	}
	boot, uptime, err := process.HostClock()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.put("health_probe", WorkerHealthProbe{Schema: 1, ObservedAt: now, BootID: boot, Uptime: uptime, ConfigHash: c.ConfigHash, BackupHealthy: true, DeliveryHealthy: true, Healthy: true}); err != nil {
		t.Fatal(err)
	}
	summary, healthy, err := evaluateWorkerHealth(c, s, time.Now().UTC())
	if err != nil || !healthy {
		t.Fatalf("baseline is not healthy: %s, %v", summary, err)
	}
	return a, s, c, r
}
