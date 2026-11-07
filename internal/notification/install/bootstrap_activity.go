package install

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// RestoreBootstrapVerification restores only the exact retained, unbound
// verification installation. The caller owns the separate desired-activity
// decision; this is not permission to adopt an unknown or retired installation.
// A service revision changes, but gateway/account identities and policy do not.
func RestoreBootstrapVerification(ctx context.Context, root, expected string) (ManagedManifest, error) {
	return newManagedInstaller().restoreBootstrapVerification(ctx, root, expected)
}

func (i managedInstaller) restoreBootstrapVerification(ctx context.Context, root, expected string) (ManagedManifest, error) {
	var zero ManagedManifest
	if !safefs.DigestPattern.MatchString(expected) {
		return zero, fmt.Errorf("exact disabled installation receipt required")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return zero, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", true, false)
	if err != nil {
		return zero, err
	}
	j, err := readManagedJournal(d)
	if err != nil {
		lock.Close()
		return zero, err
	}
	b := j.Intent.After
	resuming := j.Intent.Operation == "update" && j.Intent.Before != nil && protocol.Hash(*j.Intent.Before) == expected
	if resuming {
		b = *j.Intent.Before
	}
	if b.Root != root || protocol.Hash(b) != expected || b.Mode != "uninstalled" || b.DeliveryOwnerHash != "" || b.RecoveryCommitHash != "" || b.Commands != nil || (!resuming && j.Phase != "complete") {
		lock.Close()
		return zero, fmt.Errorf("activity restore requires the exact stopped unbound verification installation")
	}
	c, err := verifyManagedArtifacts(d, b)
	if err == nil && !c.Bootstrap {
		err = fmt.Errorf("bound consumers require coordinated activity restoration")
	}
	if err != nil {
		lock.Close()
		return zero, err
	}
	if resuming {
		defer lock.Close()
		want, after := b, j.Intent.After
		want.Mode, want.Revision, want.UnitHash = "verification", after.Revision, after.UnitHash
		want.Paths.Executable, want.Paths.Config = after.Paths.Executable, after.Paths.Config
		if protocol.Hash(want) != protocol.Hash(after) {
			return zero, fmt.Errorf("pending update differs from activity restoration")
		}
		return i.resumeLocked(ctx, d, j, lock)
	}
	binary, err := ownedManagedFile(d, "releases/"+b.Revision+"/deploy", maxManagedBinary)
	if err != nil {
		lock.Close()
		return zero, err
	}
	raw, err := ownedManagedFile(d, "releases/"+b.Revision+"/runtime.json", safefs.MaxRecordBytes)
	lock.Close()
	if err != nil {
		return zero, err
	}
	if managedDigest(binary) != b.BinaryHash || managedDigest(raw) != b.RuntimeHash {
		return zero, fmt.Errorf("retained activity input changed")
	}
	parent, err := privateManagedDir(filepath.Dir(root), false)
	if err != nil {
		return zero, err
	}
	defer parent.Close()
	if err = parent.Capacity(int64(len(binary)+len(raw)) + (32 << 20)); err != nil {
		return zero, err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return zero, err
	}
	name := ".notification-activity-" + id
	if err = unix.Mkdirat(int(parent.File().Fd()), name, 0700); err != nil {
		return zero, err
	}
	stage, err := parent.Sub(name)
	if err != nil {
		return zero, err
	}
	defer func() {
		unix.Unlinkat(int(stage.File().Fd()), "deploy", 0)
		unix.Unlinkat(int(stage.File().Fd()), "runtime.json", 0)
		stage.Close()
		unix.Unlinkat(int(parent.File().Fd()), name, unix.AT_REMOVEDIR)
	}()
	if err = frozenManagedFile(stage, "deploy", binary, 0700); err != nil {
		return zero, err
	}
	if err = frozenManagedFile(stage, "runtime.json", raw, 0600); err != nil {
		return zero, err
	}
	spec := ManagedSpec{Version: 1, Root: b.Root, State: b.Paths.State, UnitDirectory: b.UnitDirectory, RuntimeFile: filepath.Join(filepath.Dir(root), name, "runtime.json"), BinaryFile: filepath.Join(filepath.Dir(root), name, "deploy"), BinarySHA256: b.BinaryHash, DeniedPaths: b.Paths.DeniedPaths, FreshSignal: b.FreshSignal}
	// prepareExpected holds the domain lease and rechecks b before publishing
	// update intent. A concurrent change cannot slip between inspection and use.
	return i.prepareExpected(ctx, "update", spec, expected)
}
