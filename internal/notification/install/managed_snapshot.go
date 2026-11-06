package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

// managedSnapshot uses the same permanent owner and recovery checks as runtime.
// It requires an already stopped service and never restarts it or sends messages.
func (i managedInstaller) managedSnapshot(ctx context.Context, root, manifestHash, to string, dry bool) (out worker.SnapshotManifest, err error) {
	if !managedPath(root) || !managedPath(to) || !safefs.DigestPattern.MatchString(manifestHash) {
		return out, fmt.Errorf("managed snapshot requires root, manifest hash and a new absolute destination")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return out, err
	}
	defer d.Close()
	operation, err := d.Lock("operation.lock", !dry, false)
	if err != nil {
		return out, err
	}
	defer operation.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return out, err
	}
	if j.Phase != "complete" {
		return out, fmt.Errorf("finish the current installation operation before snapshot")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return out, err
	}
	m := j.Intent.After
	if protocol.Hash(m) != manifestHash || m.Mode == "uninstalled" {
		return out, fmt.Errorf("managed snapshot installation pin changed")
	}
	c, err := verifyManagedArtifacts(d, m)
	if err != nil {
		return out, err
	}
	units, err := managedUnitDirectory(m.UnitDirectory)
	if err != nil {
		return out, err
	}
	defer units.Close()
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return out, err
	}
	if !present || managedDigest(unit) != m.UnitHash {
		return out, fmt.Errorf("managed snapshot unit changed")
	}
	state, err := inspectManagedService(ctx, i.services, m, false)
	if err != nil {
		return out, err
	}
	if state.Active != "inactive" && state.Active != "failed" {
		return out, fmt.Errorf("stop the managed service before taking an offline snapshot")
	}
	if err = i.inspectDelivery(ctx, m, false); err != nil {
		return out, err
	}
	if dry {
		return out, nil
	}
	lease, err := d.Lock("installation.lock", true, false)
	if err != nil {
		return out, err
	}
	defer lease.Close()
	if err = checkManagedDirectoryPath(d, root, true); err != nil {
		return out, err
	}
	if err = managedLeaseMatches(d, "operation.lock", operation); err != nil {
		return out, err
	}
	if err = managedLeaseMatches(d, "installation.lock", lease); err != nil {
		return out, err
	}
	s, err := openManagedStore(m, c)
	if err != nil {
		return out, err
	}
	defer s.Close()
	return s.Snapshot(ctx, to)
}

func runManagedSnapshotCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker managed-snapshot", flag.ContinueOnError)
	local := f.Bool("local", false, "Use an offline managed installation")
	root := f.String("root", "", "Managed installation root")
	hash := f.String("manifest-hash", "", "Exact installation manifest hash")
	to := f.String("to", "", "New private snapshot directory")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 {
		return fmt.Errorf("managed-snapshot requires --local and no positional arguments")
	}
	m, err := newManagedInstaller().managedSnapshot(context.Background(), *root, *hash, *to, ui.DryRun)
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] managed snapshot pins validated; no state opened or snapshot written")
		return err
	}
	return json.NewEncoder(out).Encode(m)
}
