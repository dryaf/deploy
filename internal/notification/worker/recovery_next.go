package worker

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type nextRecoveryIntent struct {
	Version  int                     `json:"version"`
	Parent   ManagedDeliveryRecovery `json:"parent"`
	Revision string                  `json:"revision"`
	Journal  snapshotRecovery        `json:"journal"`
}

func nextRecoveryName(commit string) string { return "recovery-next-" + commit + ".json" }

// QuarantineNextManagedDeliveryRecovery requires the stopped managed service and
// its exclusive installation lease. Parent ownership and snapshot pins are
// explicit; no prior publication or enrollment is inferred from a path.
func QuarantineNextManagedDeliveryRecovery(ctx context.Context, path, snapshot string, c Config, snapshotID, databaseHash, revision string, parent ManagedDeliveryRecovery) (RecoveryQuarantineView, error) {
	m, err := VerifySnapshot(ctx, snapshot, c)
	if err != nil {
		return RecoveryQuarantineView{}, err
	}
	if m.ID != snapshotID || m.DatabaseHash != databaseHash {
		return RecoveryQuarantineView{}, fmt.Errorf("next recovery snapshot pins changed")
	}
	if err = prepareNextManagedRecovery(ctx, path, snapshot, c, m, parent, revision, nil); err != nil {
		return RecoveryQuarantineView{}, err
	}
	return quarantineRecoveryOwner(ctx, path, snapshot, c, snapshotID, databaseHash, &parent.Owner, revision, nil)
}

func prepareNextManagedRecovery(ctx context.Context, path, snapshot string, c Config, m SnapshotManifest, parent ManagedDeliveryRecovery, revision string, fault func(string) error) (err error) {
	if parent.Validate() != nil || m.Version != 2 || m.Identity != parent.After || !safefs.ValidID(revision, "n") {
		return fmt.Errorf("next recovery requires exact managed parent and recovered snapshot")
	}
	got, source, err := readSnapshot(ctx, snapshot, c)
	if err != nil {
		return err
	}
	defer source.Close()
	if got != m {
		return fmt.Errorf("next recovery snapshot changed")
	}
	e, err := readSnapshotRecovery(source, m, c)
	if err != nil {
		return err
	}
	if e.Publication.CommitHash != parent.CommitHash {
		return fmt.Errorf("next recovery snapshot has another parent")
	}
	d, err := privateDirectory(path)
	if err != nil {
		return err
	}
	defer d.Close()
	var intent nextRecoveryIntent
	err = readCheckpointJSON(d, nextRecoveryName(parent.CommitHash), &intent)
	if os.IsNotExist(err) {
		s, err := OpenManagedRecoveryVerification(path, c, parent.After, revision, parent.CommitHash)
		if err != nil {
			return err
		}
		defer s.Close()
		if err = s.RequireManagedDeliveryRecovery(parent, protocol.Hash(parent.Owner)); err != nil {
			return err
		}
		if err = s.RequireInactiveProducers(); err != nil {
			return err
		}
		j := snapshotRecovery{Version: 2, Before: m.Identity, After: m.Identity, Snapshot: m, Phase: "prepared"}
		if j.Operation, err = safefs.NewID("n"); err != nil {
			return err
		}
		if j.After.Recovery, err = safefs.NewID("n"); err != nil {
			return err
		}
		if err = s.dir.Mkdir("recovery/" + j.Operation); err != nil {
			return err
		}
		stage, err := s.dir.Sub("recovery/" + j.Operation)
		if err != nil {
			return err
		}
		defer stage.Close()
		if err = copySnapshotFile(source, "state.db", stage, "source.db", m); err != nil {
			return err
		}
		if err = copySnapshotFile(source, "state.db", stage, "candidate.db", m); err != nil {
			return err
		}
		archive, err := source.Sub(snapshotProofDirectory)
		if err != nil {
			return err
		}
		err = copyRecoveryProofArchive(ctx, archive, stage, c, *e)
		archive.Close()
		if err != nil {
			return err
		}
		if err = stage.WriteJSON(snapshotRecoveryFile, *e, false); err != nil {
			return err
		}
		head, err := s.commandSQLHead()
		if err != nil {
			return err
		}
		hash := ""
		if head != nil {
			hash = head.Hash
		}
		if err = s.RequireManagedCommandHistory(hash); err != nil {
			return err
		}
		if j.RuntimeHash, err = retainOriginalRuntime(ctx, s.dir, stage, c); err != nil {
			return err
		}
		intent = nextRecoveryIntent{1, parent, revision, j}
		if err = s.dir.WriteJSON(nextRecoveryName(parent.CommitHash), intent, false); err != nil {
			return err
		}
		if err = recoveryBoundary(fault, "next_intent"); err != nil {
			return err
		}
		return finishNextManagedRecovery(ctx, s.dir, c, intent, fault)
	}
	if err != nil {
		return err
	}
	if intent.Version != 1 || intent.Parent != parent || intent.Revision != revision || intent.Journal.Snapshot != m {
		return fmt.Errorf("next recovery intent changed")
	}
	lease, err := d.Lock("worker.lock", true, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	return finishNextManagedRecovery(ctx, d, c, intent, fault)
}

func finishNextManagedRecovery(ctx context.Context, d *safefs.Directory, c Config, intent nextRecoveryIntent, fault func(string) error) error {
	j := intent.Journal
	if intent.Version != 1 || j.validate(c) != nil || j.Phase != "prepared" || j.Before != intent.Parent.After {
		return fmt.Errorf("invalid next recovery intent")
	}
	stage, err := d.Sub("recovery/" + j.Operation)
	if err != nil {
		return err
	}
	defer stage.Close()
	if err = verifyOriginalRuntime(ctx, stage, j, c); err != nil {
		return err
	}
	e, err := retainedSnapshotRecovery(ctx, stage, j.Snapshot, c)
	if err != nil {
		return err
	}
	if e == nil || e.Publication.CommitHash != intent.Parent.CommitHash {
		return fmt.Errorf("next recovery lost its ancestor")
	}
	archive, err := stage.Sub(snapshotProofDirectory)
	if err != nil {
		return err
	}
	defer archive.Close()
	var old, current snapshotRecovery
	if err = readCheckpointJSON(archive, "recovery.json", &old); err != nil {
		return err
	}
	if err = readCheckpointJSON(d, "recovery.json", &current); err != nil {
		return err
	}
	if current != old && (current.binding() != j.binding() || current.Snapshot != j.Snapshot || current.validate(c) != nil) {
		return fmt.Errorf("next recovery journal changed")
	}
	if current.binding() == j.binding() && current.Phase == "quarantined" {
		return finishSnapshotRecovery(ctx, d, c, &current, fault)
	}
	var identity protocol.Identity
	if err = readCheckpointJSON(d, "identity.json", &identity); err != nil {
		return err
	}
	if identity != j.Before && identity != j.After {
		return fmt.Errorf("next recovery identity changed")
	}
	if err = d.WriteJSON("identity.json", j.After, true); err != nil {
		return err
	}
	if err = recoveryBoundary(fault, "next_identity"); err != nil {
		return err
	}
	// Every removed root slot has an independently verified immutable copy. On
	// resume, a missing slot is allowed only under this exact durable intent.
	for _, name := range []string{recoveryVerificationComplete, recoveryVerificationIntent, publicationCommitComplete, publicationCommitSeal, publicationExchange, publicationCommitIntent} {
		if err = retireAncestorSlot(d, archive, name, c.MaxDatabaseBytes); err != nil {
			return err
		}
		if err = recoveryBoundary(fault, "next_retire_"+name); err != nil {
			return err
		}
	}
	if current == old {
		if err = d.WriteJSON("recovery.json", j, true); err != nil {
			return err
		}
	} else {
		j = current
	}
	if err = recoveryBoundary(fault, "next_journal"); err != nil {
		return err
	}
	return finishSnapshotRecovery(ctx, d, c, &j, fault)
}

func retireAncestorSlot(d, archive *safefs.Directory, name string, limit int64) error {
	f, err := snapshotFile(d, name, limit)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	got, err := safefs.DigestFile(f)
	f.Close()
	if err != nil {
		return err
	}
	f, err = snapshotFile(archive, name, limit)
	if err != nil {
		return err
	}
	want, err := safefs.DigestFile(f)
	f.Close()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("ancestor root slot changed: %s", name)
	}
	if err = unix.Unlinkat(int(d.File().Fd()), name, 0); err != nil {
		return err
	}
	return d.File().Sync()
}
