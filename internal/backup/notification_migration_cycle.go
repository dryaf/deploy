package backup

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const migrationCycleKey = "notification_migration_cycle"
const migrationCyclePendingKey = "notification_migration_cycle_pending"

func requireMigrationCycleCapability(ctx context.Context, services backupCutoverServices, paths WorkerUnitPaths, hash string) error {
	capable, ok := services.(interface {
		PrepareMigrationCycle(context.Context, WorkerUnitPaths, string) error
	})
	if !ok {
		return fmt.Errorf("migration cycle helper capability unavailable")
	}
	return capable.PrepareMigrationCycle(ctx, paths, hash)
}

func requireNoPendingMigrationCycle(tx *sql.Tx) error {
	var present bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM meta WHERE key=?)", migrationCyclePendingKey).Scan(&present); err != nil {
		return err
	}
	if present {
		return fmt.Errorf("notification migration cycle is pending; resume the exact cycle specification")
	}
	return nil
}

func pendingCycleOwner(tx *sql.Tx, owner, previous backupNotificationOwner) (bool, error) {
	var pending backupMigrationCycle
	err := backupNotificationMeta(tx, migrationCyclePendingKey, &pending)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fenced := pending.Before
	fenced.Mode = "fenced"
	if pending.validate() != nil || owner != fenced || owner.CutoverHash != previous.CutoverHash || owner.Socket != previous.Socket {
		return false, fmt.Errorf("pending migration owner changed")
	}
	binding, err := migrationBindingTo(tx, owner.Binding, previous.Binding)
	return binding == previous.Binding && err == nil, err
}

type backupMigrationCycleSpec struct {
	Version    int                            `json:"version"`
	Managed    install.ManagedProducerRequest `json:"managed"`
	HelperHash string                         `json:"helper_hash"`
	ConfigHash string                         `json:"config_hash"`
	OwnerHash  string                         `json:"owner_hash"`
	Reason     string                         `json:"reason"`
}

type backupMigrationCycle struct {
	Version     int                         `json:"version"`
	Parent      string                      `json:"parent,omitempty"`
	Spec        backupMigrationCycleSpec    `json:"spec"`
	Before      backupNotificationOwner     `json:"before"`
	Remigration *backupRemigration          `json:"remigration,omitempty"`
	Rollback    *backupLegacyRollbackIntent `json:"rollback,omitempty"`
	Metadata    [][2]string                 `json:"metadata"`
	Pages       []string                    `json:"pages,omitempty"`
	Exports     uint64                      `json:"exports"`
}

func migrationArtifact(parent, name string) string {
	if parent == "" {
		return name
	}
	return filepath.Join("notification-migration-cycles", parent, name)
}

func migrationCycleHead(tx *sql.Tx) (string, error) {
	var head string
	err := backupNotificationMeta(tx, migrationCycleKey, &head)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !safefs.DigestPattern.MatchString(head) {
		return "", fmt.Errorf("invalid backup migration cycle head")
	}
	return head, nil
}

func currentMigrationArtifact(state *backupState, name string) (string, error) {
	tx, err := state.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	head, err := migrationCycleHead(tx)
	if err != nil {
		return "", err
	}
	return migrationArtifact(head, name), nil
}

func effectiveBackupRemigration(tx *sql.Tx) (*backupRemigration, error) {
	r, err := readBackupRemigration(tx)
	if err != nil || r != nil {
		return r, err
	}
	history, err := readBackupMigrationCycles(tx, nil)
	if err != nil || len(history) == 0 {
		return nil, err
	}
	return history[0].Remigration, nil
}

func backupRemigrationByHash(tx *sql.Tx, hash string) (*backupRemigration, error) {
	r, err := readBackupRemigration(tx)
	if err != nil {
		return nil, err
	}
	if r != nil && notify.Hash(*r) == hash {
		return r, nil
	}
	history, err := readBackupMigrationCycles(tx, nil)
	if err != nil {
		return nil, err
	}
	for _, cycle := range history {
		if r := cycle.Remigration; r != nil && notify.Hash(*r) == hash {
			return r, nil
		}
	}
	return nil, fmt.Errorf("remigration source authority lost")
}

func migrationBindingTo(tx *sql.Tx, b, target backupNotificationBinding) (backupNotificationBinding, error) {
	history, err := readBackupMigrationCycles(tx, nil)
	if err != nil {
		return b, err
	}
	current, err := readBackupRemigration(tx)
	if err != nil {
		return b, err
	}
	records := []*backupRemigration{current}
	for _, cycle := range history {
		records = append(records, cycle.Remigration)
	}
	for range len(records) + 1 {
		if b == target {
			return b, nil
		}
		changed := false
		for _, r := range records {
			if r != nil && b == r.After.Binding {
				b, changed = r.Before.Binding, true
				break
			}
		}
		if !changed {
			return b, nil
		}
	}
	return b, fmt.Errorf("cyclic backup migration lineage")
}

func recoveryOwnerDescendsThroughCycles(tx *sql.Tx, owner, previous backupNotificationOwner) (bool, error) {
	if owner.Schema != 3 {
		return false, nil
	}
	history, err := readBackupMigrationCycles(tx, nil)
	if err != nil {
		return false, err
	}
	if len(history) == 0 || owner.CutoverHash != previous.CutoverHash || owner.Socket != previous.Socket {
		return false, nil
	}
	binding, err := migrationBindingTo(tx, owner.Binding, previous.Binding)
	return err == nil && binding == previous.Binding, err
}

// Recovery and notification migrations may alternate. Follow exact retained
// before/after bindings instead of assuming all migrations precede all restores.
func backupHistoricalBinding(state *backupState, b backupNotificationBinding) (backupNotificationBinding, error) {
	tx, err := state.db.Begin()
	if err != nil {
		return b, err
	}
	defer tx.Rollback()
	d, err := safefs.Open(state.directory)
	if err != nil {
		return b, err
	}
	defer d.Close()
	return backupHistoricalBindingTo(tx, d, b, backupNotificationBinding{})
}

func backupHistoricalBindingTo(tx *sql.Tx, d *safefs.Directory, b, target backupNotificationBinding) (backupNotificationBinding, error) {
	history, err := readBackupMigrationCycles(tx, d)
	if err != nil {
		return b, err
	}
	var steps [][2]backupNotificationBinding
	current, err := readBackupRemigration(tx)
	if err != nil {
		return b, err
	}
	if current != nil {
		steps = append(steps, [2]backupNotificationBinding{current.After.Binding, current.Before.Binding})
	}
	for _, cycle := range history {
		if r := cycle.Remigration; r != nil {
			steps = append(steps, [2]backupNotificationBinding{r.After.Binding, r.Before.Binding})
		}
	}
	var journal backupNotificationRecovery
	if err = backupNotificationMeta(tx, "notification_recovery", &journal); err == nil {
		activation, err := readRecoveryActivation(tx, false)
		if err != nil && err != sql.ErrNoRows {
			return b, err
		}
		if err == nil {
			steps = append(steps, [2]backupNotificationBinding{activation.After.Binding, journal.Binding})
		}
		ancestors, _, err := readBackupRecoveryAncestors(tx, journal, d)
		if err != nil {
			return b, err
		}
		for _, a := range ancestors {
			steps = append(steps, [2]backupNotificationBinding{a.Activation.After.Binding, a.Journal.Binding})
		}
	} else if err != sql.ErrNoRows {
		return b, err
	}
	for range len(steps) + 1 {
		if b == target {
			return b, nil
		}
		changed := false
		for _, step := range steps {
			if b == step[0] {
				b, changed = step[1], true
				break
			}
		}
		if !changed {
			return b, nil
		}
	}
	return b, fmt.Errorf("cyclic backup ownership history")
}

func matchCycleCutover(state *backupState, original backupCutoverIntent, spec backupCutoverSpec) error {
	if spec.Version != 4 {
		return matchCutoverContinuation(original, spec)
	}
	tx, err := state.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	history, err := readBackupMigrationCycles(tx, nil)
	if err != nil {
		return err
	}
	if len(history) == 0 {
		return fmt.Errorf("cutover requires a prepared migration cycle")
	}
	r := history[0]
	b := r.Before.Binding
	if spec.Predecessor != notify.Hash(r) || r.Before.CutoverHash != notify.Hash(original) || spec.Managed.Root != original.Spec.Managed.Root || spec.Managed.Generation != b.Generation || spec.Managed.Producer != b.Producer || spec.Managed.Route != b.Route || spec.EvidenceHash != original.Spec.EvidenceHash || notify.Hash(spec.Decisions) != notify.Hash(original.Spec.Decisions) {
		return fmt.Errorf("cycle cutover changed retained authority")
	}
	return nil
}

func (r backupMigrationCycle) validate() error {
	s := r.Spec
	if r.Version != 1 || s.Version != 1 || s.Managed.Validate() != nil || s.Managed.Revoke || s.Managed.LegacyGeneration != "" || !safefs.DigestPattern.MatchString(s.ConfigHash) || !safefs.DigestPattern.MatchString(s.HelperHash) || s.OwnerHash != notify.Hash(r.Before) || r.Before.Mode != "shared" || !safefs.DigestPattern.MatchString(r.Before.CutoverHash) || s.Managed.Producer != r.Before.Binding.Producer || s.Managed.Generation != r.Before.Binding.Generation || s.Managed.Route != r.Before.Binding.Route || s.Reason == "" || len(s.Reason) > 512 || r.Parent != "" && !safefs.DigestPattern.MatchString(r.Parent) {
		return fmt.Errorf("invalid backup migration cycle authority")
	}
	if r.Remigration != nil {
		if r.Rollback == nil || r.Remigration.validate() != nil || r.Remigration.Spec.RollbackHash != notify.Hash(*r.Rollback) || r.Remigration.Before.CutoverHash != r.Before.CutoverHash {
			return fmt.Errorf("migration cycle lost predecessor rollback")
		}
	} else if r.Rollback != nil {
		return fmt.Errorf("migration cycle has an unfinished legacy rollback")
	}
	values := map[string]string{}
	for _, item := range r.Metadata {
		if _, ok := values[item[0]]; ok {
			return fmt.Errorf("duplicate migration cycle metadata")
		}
		values[item[0]] = item[1]
	}
	if r.Remigration != nil {
		if values[remigrationKey] != mustCutoverJSON(*r.Remigration) || values[remigrationKey+"_complete"] != mustCutoverJSON(notify.Hash(*r.Remigration)) || values["notification_legacy_rollback"] != mustCutoverJSON(notify.Hash(*r.Rollback)) || values["notification_legacy_rollback_revoked"] != values["notification_legacy_rollback"] {
			return fmt.Errorf("migration cycle metadata lost completed predecessor")
		}
		for source, decision := range r.Remigration.Spec.Cutover.Decisions {
			if values[remigrationSourcePrefix+source] != mustCutoverJSON(decision) {
				return fmt.Errorf("migration cycle lost source evidence")
			}
		}
	}
	return nil
}

// Every cycle retains its original artifact names. Advancing the SQL head never
// overwrites a previous rollback or remigration, including its source decisions.
func readBackupMigrationCycles(tx *sql.Tx, d *safefs.Directory) ([]backupMigrationCycle, error) {
	head, err := migrationCycleHead(tx)
	if err != nil {
		return nil, err
	}
	var history []backupMigrationCycle
	for head != "" {
		if len(history) >= 32 {
			return nil, fmt.Errorf("backup migration cycle history limit reached")
		}
		var raw string
		if err = tx.QueryRow("SELECT body FROM backup_notification_migration_cycles WHERE hash=?", head).Scan(&raw); err != nil {
			return nil, err
		}
		var r backupMigrationCycle
		if err = notify.Decode([]byte(raw), &r, safefs.MaxRecordBytes); err != nil {
			return nil, err
		}
		if notify.Hash(r) != head || r.validate() != nil {
			return nil, fmt.Errorf("backup migration cycle history changed")
		}
		if _, err = readMigrationExports(tx, d, r); err != nil {
			return nil, err
		}
		if d != nil {
			var file backupMigrationCycle
			if err = d.ReadJSON(migrationArtifact(head, "cycle.json"), &file); err != nil {
				return nil, err
			}
			if notify.Hash(file) != head {
				return nil, fmt.Errorf("backup migration cycle file differs from SQL")
			}
			if r.Remigration != nil {
				var remigration backupRemigration
				var rollback backupLegacyRollbackIntent
				var complete string
				if err = d.ReadJSON(migrationArtifact(r.Parent, remigrationFile), &remigration); err != nil {
					return nil, err
				}
				if err = d.ReadJSON(migrationArtifact(r.Parent, remigrationFile+".complete"), &complete); err != nil {
					return nil, err
				}
				if err = d.ReadJSON(migrationArtifact(r.Parent, legacyRollbackFile), &rollback); err != nil {
					return nil, err
				}
				if notify.Hash(remigration) != notify.Hash(*r.Remigration) || complete != notify.Hash(remigration) || notify.Hash(rollback) != notify.Hash(*r.Rollback) {
					return nil, fmt.Errorf("backup migration cycle lost independent predecessor proofs")
				}
			}
		}
		history = append(history, r)
		head = r.Parent
	}
	return history, nil
}
