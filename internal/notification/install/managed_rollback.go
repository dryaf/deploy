package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// Rollback re-releases the exact retained executable as a new revision through
// the normal update transaction. Its journal is intentionally an ordinary update:
// compatible retained recovery helpers can resume it without a new journal type.
// No queue snapshot, schema, credentials or producer ownership is rolled back.
type managedRollback struct {
	Current  ManagedManifest `json:"current"`
	Selected ManagedManifest `json:"selected"`
}

func reviewManagedRollback(root, revision string) (review managedRollback, binary, runtime []byte, err error) {
	if !managedPath(root) || !safefs.ValidID(revision, "n") {
		return review, nil, nil, fmt.Errorf("rollback requires an absolute root and retained revision ID")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return review, nil, nil, err
	}
	defer d.Close()
	lease, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return review, nil, nil, err
	}
	defer lease.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return review, nil, nil, err
	}
	if j.Phase != "complete" {
		return review, nil, nil, fmt.Errorf("rollback requires a completed lifecycle operation; resume or uninstall unfinished work first")
	}
	review.Current = j.Intent.After
	if review.Current.Root != root || (revision == review.Current.Revision && review.Current.Mode != "uninstalled") {
		return review, nil, nil, fmt.Errorf("select a different retained revision of this installation")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return review, nil, nil, err
	}
	if _, err = verifyManagedArtifacts(d, review.Current); err != nil {
		return review, nil, nil, err
	}
	ops, err := d.Sub("operations")
	if err != nil {
		return review, nil, nil, err
	}
	defer ops.Close()
	names, err := ops.File().Readdirnames(33)
	if err != nil && !errors.Is(err, io.EOF) {
		return review, nil, nil, err
	}
	if len(names) > 32 {
		return review, nil, nil, fmt.Errorf("installation operation history exceeds limit")
	}
	found := false
	for _, name := range names {
		if !safefs.ValidID(name, "n") {
			return review, nil, nil, fmt.Errorf("unknown installation operation history")
		}
		var intent managedIntent
		if err = ops.ReadJSON(name+"/intent.json", &intent); err != nil {
			return review, nil, nil, err
		}
		if intent.ID != name {
			return review, nil, nil, fmt.Errorf("installation intent key changed")
		}
		if err = (managedJournal{Intent: intent, Phase: "prepared"}).validate(); err != nil {
			return review, nil, nil, err
		}
		for _, m := range []*ManagedManifest{intent.Before, &intent.After} {
			if m == nil || m.Revision != revision {
				continue
			}
			// Uninstall changes only the mode of a retained manifest.
			candidate := *m
			candidate.Mode = "verification"
			// Renewal changes the audit pin, not the executable revision. A
			// binary rollback always retains the current Telegram evidence.
			candidate.TelegramRenewalID = review.Current.TelegramRenewalID
			if found && protocol.Hash(candidate) != protocol.Hash(review.Selected) {
				return review, nil, nil, fmt.Errorf("conflicting retained revision manifests")
			}
			review.Selected = candidate
			found = true
		}
	}
	if !found {
		return review, nil, nil, fmt.Errorf("revision has no retained installation intent")
	}
	a, b := review.Current, review.Selected
	if a.Installation != b.Installation || a.Root != b.Root || a.Paths.State != b.Paths.State || a.UnitDirectory != b.UnitDirectory || a.Identity != b.Identity || a.RecoveryCommitHash != b.RecoveryCommitHash || a.ConfigHash != b.ConfigHash || protocol.Hash(a.SignalVerification) != protocol.Hash(b.SignalVerification) || a.Schema != b.Schema || a.HelperHash != b.HelperHash || a.HelperRevision != b.HelperRevision || protocol.Hash(a.Paths.DeniedPaths) != protocol.Hash(b.Paths.DeniedPaths) {
		return review, nil, nil, fmt.Errorf("rollback cannot cross configuration, schema, identity or recovery-helper changes")
	}
	if _, err = verifyManagedArtifacts(d, b); err != nil {
		return review, nil, nil, err
	}
	binary, err = ownedManagedFile(d, "releases/"+revision+"/deploy", maxManagedBinary)
	if err != nil {
		return review, nil, nil, err
	}
	runtime, err = ownedManagedFile(d, "releases/"+a.Revision+"/runtime.json", safefs.MaxRecordBytes)
	if err != nil {
		return review, nil, nil, err
	}
	// Recheck bytes after collecting the retained artifacts; prepareExpected pins
	// the current installation again when it obtains the exclusive operation lease.
	if managedDigest(binary) != b.BinaryHash || managedDigest(runtime) != a.RuntimeHash {
		return review, nil, nil, fmt.Errorf("retained rollback input changed")
	}
	return review, binary, runtime, nil
}

func (i managedInstaller) rollback(ctx context.Context, root, revision string) (ManagedManifest, error) {
	var zero ManagedManifest
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	review, binary, runtime, err := reviewManagedRollback(root, revision)
	if err != nil {
		return zero, err
	}
	if review.Current.Mode == "verification" && review.Current.BinaryHash == review.Selected.BinaryHash {
		return review.Current, nil
	}
	if err = i.boundary("rollback_reviewed"); err != nil {
		return zero, err
	}
	parent, err := privateManagedDir(filepath.Dir(root), false)
	if err != nil {
		return zero, err
	}
	defer parent.Close()
	if err = parent.Capacity(int64(len(binary)+len(runtime)) + (32 << 20)); err != nil {
		return zero, err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return zero, err
	}
	name := ".notification-rollback-" + id
	if err = unix.Mkdirat(int(parent.File().Fd()), name, 0700); err != nil {
		return zero, err
	}
	stage, err := parent.Sub(name)
	if err != nil {
		return zero, err
	}
	defer func() {
		// Remove only the two files we created, never recursively delete an
		// unexpected directory tree if the staging path was replaced.
		unix.Unlinkat(int(stage.File().Fd()), "deploy", 0)
		unix.Unlinkat(int(stage.File().Fd()), "runtime.json", 0)
		stage.Close()
		unix.Unlinkat(int(parent.File().Fd()), name, unix.AT_REMOVEDIR)
	}()
	if err = frozenManagedFile(stage, "deploy", binary, 0700); err != nil {
		return zero, err
	}
	if err = frozenManagedFile(stage, "runtime.json", runtime, 0600); err != nil {
		return zero, err
	}
	a := review.Current
	spec := ManagedSpec{Version: managedVersion, Root: root, State: a.Paths.State, UnitDirectory: a.UnitDirectory, RuntimeFile: filepath.Join(filepath.Dir(root), name, "runtime.json"), BinaryFile: filepath.Join(filepath.Dir(root), name, "deploy"), BinarySHA256: review.Selected.BinaryHash, DeniedPaths: a.Paths.DeniedPaths}
	if err = i.boundary("rollback_staged"); err != nil {
		return zero, err
	}
	return i.prepareExpected(ctx, "update", spec, protocol.Hash(a))
}
