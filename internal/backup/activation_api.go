package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/source"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type ActivationResult struct {
	State     string `json:"state"`
	Unchanged bool   `json:"unchanged"`
}
type declaredActivation struct {
	Version  int      `json:"version"`
	Desired  string   `json:"desired"`
	Runtime  string   `json:"runtime"`
	Proof    string   `json:"proof"`
	Complete bool     `json:"complete"`
	Units    []string `json:"units"`
}
type activationDependencies struct {
	validate    func(WorkerInstallation, *backupState) ([]string, error)
	unitsActive func(context.Context, []string) (bool, error)
	enable      func(context.Context, []string) error
}

func requireSettledDeclaredActivation(home string) error {
	s, err := openInstalledBackupState(home, false)
	if err != nil {
		return err
	}
	defer s.Close()
	return requireNoPendingDeclaredActivation(s)
}

func requireNoPendingDeclaredActivation(s *backupState) error {
	var retained declaredActivation
	err := s.get("declared_activation", &retained)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if retained.Version != 1 || !retained.Complete {
		return fmt.Errorf("finish the pending declared activation before changing this installation")
	}
	return nil
}

func ActivateDeclaredWorker(ctx context.Context, home, desired, helper string, targets ...SetupSourceTarget) (ActivationResult, error) {
	return activateDeclaredWorker(ctx, home, desired, helper, activationDependencies{
		validate: func(c WorkerInstallation, s *backupState) ([]string, error) {
			return validateWorkerActivation(c, s, requireSharedBackupReportUnits, source.QualifyRestrictedSource)
		},
		unitsActive: func(ctx context.Context, units []string) (bool, error) { return declaredUnitsActive(ctx, home, units) },
		enable: func(ctx context.Context, units []string) error {
			cmd := exec.Command("systemctl", append([]string{"--user", "enable", "--now"}, units...)...)
			cmd.Stdout, cmd.Stderr = &process.Buffer{Limit: 8192}, &process.Buffer{Limit: 8192}
			if err := process.RunContext(ctx, cmd, 30*time.Second, 5*time.Second); err != nil {
				return fmt.Errorf("activation outcome unconfirmed; repeat activate to reconcile the retained unit set")
			}
			return nil
		},
	}, targets...)
}

func activateDeclaredWorker(ctx context.Context, home, desired, helper string, deps activationDependencies, targets ...SetupSourceTarget) (out ActivationResult, err error) {
	err = withDeclaredWorker(ctx, home, desired, helper, func(_ *safefs.Directory, j WorkerSetupJournal, c WorkerInstallation, s *backupState, observed SetupObservation) error {
		if err := requireDeclaredSourceTargets(c, targets); err != nil {
			return err
		}
		var retained declaredActivation
		e := s.get("declared_activation", &retained)
		exists := e == nil
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if exists && (retained.Version != 1 || !safefs.DigestPattern.MatchString(retained.Desired) || !safefs.DigestPattern.MatchString(retained.Runtime) || !safefs.DigestPattern.MatchString(retained.Proof)) {
			return fmt.Errorf("invalid retained activation intent")
		}
		// A completed prior policy can be replaced only after an actual staged
		// update. A pending activation must first reconcile its original policy.
		if exists && (retained.Desired != desired || retained.Runtime != notify.Hash(c)) {
			if !retained.Complete || observed.Installation != "staged-awaiting-qualification" {
				return fmt.Errorf("activation policy changed; reconcile the original operation first")
			}
			exists = false
		}
		owner, e := currentSharedBackupOwner(s)
		if e != nil {
			return e
		}
		units := workerPolicyActivationUnits(c, owner != nil)
		if exists && notify.Hash(retained.Units) != notify.Hash(units) {
			return fmt.Errorf("activation unit ownership changed")
		}
		if exists && retained.Complete {
			if observed.Installation != "active" {
				return fmt.Errorf("activation marker changed; recovery review required")
			}
			var recorded backupQualificationBindingRecord
			binding, e := backupQualificationBinding(j, c)
			if e != nil {
				return e
			}
			if e = s.get("qualification_binding", &recorded); e != nil || recorded.Version != 1 || recorded.Binding != binding || recorded.Proof != retained.Proof {
				return fmt.Errorf("activated qualification inputs changed; review credential rotation before reconciliation")
			}
			active, e := deps.unitsActive(ctx, units)
			if e != nil {
				return e
			}
			if !active {
				return fmt.Errorf("activated units are no longer enabled and active; inspect service state before recovery")
			}
			out = ActivationResult{State: "active", Unchanged: true}
			return nil
		}
		if observed.Installation == "active" && !exists {
			return fmt.Errorf("active worker has no declarative activation receipt; adoption review required")
		}
		var proof WorkerQualification
		if e = s.get("qualification", &proof); e != nil {
			return e
		}
		if !currentBackupQualification(proof, c, time.Now().UTC()) {
			return fmt.Errorf("activation requires current passing evidence for this exact image and installation; run qualify")
		}
		binding, e := backupQualificationBinding(j, c)
		if e != nil {
			return e
		}
		var recorded backupQualificationBindingRecord
		if e = s.get("qualification_binding", &recorded); e != nil || recorded.Version != 1 || recorded.Binding != binding || recorded.Proof != notify.Hash(proof) {
			return fmt.Errorf("qualification inputs or artifacts changed; run qualify for the current declaration")
		}
		if exists && retained.Proof != notify.Hash(proof) {
			return fmt.Errorf("qualification changed during activation")
		}
		if observed.SharedReportingPending {
			return fmt.Errorf("complete shared reporting connection before schedule activation")
		}
		if !exists {
			units, e = deps.validate(c, s)
			if e != nil {
				return e
			}
			retained = declaredActivation{Version: 1, Desired: desired, Runtime: notify.Hash(c), Proof: notify.Hash(proof), Units: units}
			if e = s.put("declared_activation", retained); e != nil {
				return e
			}
		}
		active, e := deps.unitsActive(ctx, units)
		if e != nil {
			return e
		}
		if !active {
			// Revalidate recent run, source restrictions and monitoring before
			// any retry that still needs to start units. Already active units
			// are not restarted by systemctl enable --now.
			if _, e = deps.validate(c, s); e != nil {
				return e
			}
			if e = deps.enable(ctx, units); e != nil {
				return e
			}
			active, e = deps.unitsActive(ctx, units)
			if e != nil {
				return e
			}
			if !active {
				return fmt.Errorf("activation unit state is unconfirmed; inspect and repeat activate")
			}
		}
		retained.Complete = true
		if e = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
			if e := storeBackupEvaluationMeta(tx, "installation", "active"); e != nil {
				return e
			}
			return storeBackupEvaluationMeta(tx, "declared_activation", retained)
		}); e != nil {
			return e
		}
		out = ActivationResult{State: "active"}
		return nil
	})
	return
}

func declaredUnitsActive(ctx context.Context, home string, units []string) (bool, error) {
	paths, err := lookupInstalledBackupPaths(home)
	if err != nil {
		return false, err
	}
	all := true
	for _, unit := range units {
		cmd := exec.Command("systemctl", "--user", "show", unit, "--property=LoadState,ActiveState,UnitFileState,FragmentPath,DropInPaths,Names")
		var out, errs process.Buffer
		out.Limit, errs.Limit = 4096, 4096
		cmd.Stdout, cmd.Stderr = &out, &errs
		if err := process.RunContext(ctx, cmd, 10*time.Second, time.Second); err != nil {
			return false, fmt.Errorf("activation unit inspection unavailable")
		}
		values := map[string]string{}
		for _, line := range strings.Split(out.String(), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			if _, duplicate := values[key]; duplicate {
				return false, fmt.Errorf("ambiguous activation unit state")
			}
			values[key] = value
		}
		if len(values) != 6 || values["LoadState"] != "loaded" || values["ActiveState"] == "" || values["UnitFileState"] == "" {
			return false, fmt.Errorf("activation unit is unavailable")
		}
		if values["FragmentPath"] != filepath.Join(paths.UnitDirectory, unit) || values["DropInPaths"] != "" || values["Names"] != unit {
			return false, fmt.Errorf("activation unit has a foreign fragment, alias or drop-in; review service ownership")
		}
		all = all && values["ActiveState"] == "active" && values["UnitFileState"] == "enabled"
	}
	return all, nil
}
