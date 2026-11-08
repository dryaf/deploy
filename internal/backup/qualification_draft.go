package backup

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type backupQualificationObservation struct {
	Result       string    `json:"result"`
	EvidenceHash string    `json:"evidence_hash"`
	ObservedAt   time.Time `json:"observed_at"`
}
type backupQualificationDraft struct {
	Version   int                                       `json:"version"`
	ID        string                                    `json:"id"`
	Binding   string                                    `json:"binding"`
	Previous  string                                    `json:"previous"`
	CreatedAt time.Time                                 `json:"created_at"`
	Recorded  bool                                      `json:"recorded"`
	Checks    map[string]backupQualificationObservation `json:"checks"`
}

type backupQualificationBindingRecord struct {
	Version int    `json:"version"`
	Binding string `json:"binding"`
	Proof   string `json:"proof"`
}

func backupQualificationBinding(j WorkerSetupJournal, c WorkerInstallation) (string, error) {
	inputs := map[string]string{}
	paths := map[string]string{"backup_monitor": c.Policy.Monitoring.BackupPingURLFile, "delivery_monitor": c.Policy.Monitoring.DailyDeliveryPingURLFile}
	if p := c.Policy.Notifications.Telegram; p != nil {
		paths["telegram_token"], paths["telegram_chat"] = p.TokenFile, p.ChatIDFile
	}
	for name, path := range paths {
		if path == "" {
			continue
		}
		value, err := notification.ReadWorkerSecret(path)
		if err != nil {
			return "", fmt.Errorf("backup qualification private input unavailable: %s", name)
		}
		inputs[name] = notify.Hash(value)
	}
	binding := notify.Hash(struct {
		Setup        string
		Declaration  *SetupDeclaration
		Runtime      WorkerInstallation
		Keys, Inputs map[string]string
	}{j.ID, j.Declaration, c, j.SourceKeys, inputs})
	if j.Activity != nil && j.Activity.Version == 2 && j.Activity.Setup == j.RequestHash {
		binding = notify.Hash(struct{ Policy, Activity string }{binding, notify.Hash(*j.Activity)})
	}
	return binding, nil
}

// QualifyDeclaredWorker records operator observations only. The target creates
// the installation-bound report and keeps partial sessions in its existing DB.
// No backup, restore test, provider send or schedule activation occurs here.
func QualifyDeclaredWorker(ctx context.Context, home, desired, helper, action string, observation install.QualificationObservation, targets ...SetupSourceTarget) (out install.QualificationSession, err error) {
	err = withDeclaredWorker(ctx, home, desired, helper, func(root *safefs.Directory, j WorkerSetupJournal, c WorkerInstallation, state *backupState, observed SetupObservation) error {
		if err := requireDeclaredSourceTargets(c, targets); err != nil {
			return err
		}
		if e := requireNoPendingDeclaredActivation(state); e != nil {
			return e
		}
		if observed.Installation != "staged-awaiting-qualification" {
			return fmt.Errorf("qualification requires a staged installation; inspect the active worker before changing its evidence")
		}
		return backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			var e error
			out, e = backupQualificationDraftTx(tx, j, c, action, observation, time.Now().UTC())
			return e
		})
	})
	return
}

func backupQualificationDraftTx(tx *sql.Tx, j WorkerSetupJournal, c WorkerInstallation, action string, observation install.QualificationObservation, now time.Time) (out install.QualificationSession, err error) {
	if action != "begin" && action != "observe" && action != "commit" {
		return out, fmt.Errorf("unknown backup qualification action")
	}
	if observation.LEDVersion != "" || observation.LEDConfigHash != "" || action == "begin" && observation != (install.QualificationObservation{}) || action == "commit" && (observation.Scope != "" || observation.ID != "" || observation.Result != "" || observation.EvidenceHash != "") {
		return out, fmt.Errorf("unexpected backup qualification observation fields")
	}
	// Include the exact runtime, helper, desired policy and source key pins.
	binding, err := backupQualificationBinding(j, c)
	if err != nil {
		return out, err
	}
	var proof WorkerQualification
	if e := backupEvaluationMeta(tx, "qualification", &proof); e != nil && e != sql.ErrNoRows {
		return out, e
	}
	var draft backupQualificationDraft
	e := backupEvaluationMeta(tx, "qualification_draft", &draft)
	exists := e == nil
	if e != nil && e != sql.ErrNoRows {
		return out, e
	}
	if exists && (draft.Version != 1 || !safefs.ValidID(draft.ID, "n") || !safefs.DigestPattern.MatchString(draft.Binding) || !safefs.DigestPattern.MatchString(draft.Previous) || draft.CreatedAt.IsZero()) {
		return out, fmt.Errorf("invalid retained backup qualification draft")
	}
	if exists {
		if draft.Checks == nil || len(draft.Checks) > 16 {
			return out, fmt.Errorf("invalid retained backup observations")
		}
		for _, check := range draft.Checks {
			if (check.Result != "passed" && check.Result != "failed") || !safefs.DigestPattern.MatchString(check.EvidenceHash) || check.ObservedAt.Before(draft.CreatedAt) || check.ObservedAt.After(now) {
				return out, fmt.Errorf("invalid retained backup observation evidence")
			}
		}
	}
	current := exists && draft.Binding == binding && !now.Before(draft.CreatedAt) && now.Sub(draft.CreatedAt) <= 7*24*time.Hour
	if current {
		head := draft.Previous
		if draft.Recorded {
			head = notify.Hash(backupDraftProof(draft, c))
		}
		current = head == notify.Hash(proof)
	}
	if current && draft.Recorded {
		var recorded backupQualificationBindingRecord
		if err = backupEvaluationMeta(tx, "qualification_binding", &recorded); err != nil || recorded.Version != 1 || recorded.Binding != binding || recorded.Proof != notify.Hash(proof) {
			return out, fmt.Errorf("recorded backup qualification lost its input binding; evidence review required")
		}
	}
	if action == "begin" {
		if !current || draft.Recorded && !backupDraftComplete(draft, c) {
			var count int
			if err = tx.QueryRow("SELECT count(*) FROM meta WHERE key LIKE 'qualification_session:%'").Scan(&count); err != nil {
				return out, err
			}
			if count >= 64 {
				return out, fmt.Errorf("qualification history limit reached; retain and review existing evidence")
			}
			id, e := safefs.NewID("n")
			if e != nil {
				return out, e
			}
			next := backupQualificationDraft{Version: 1, ID: id, Binding: binding, Previous: notify.Hash(proof), CreatedAt: now, Checks: map[string]backupQualificationObservation{}}
			if current {
				next.CreatedAt = draft.CreatedAt
				for key, check := range draft.Checks {
					if check.Result == "passed" {
						next.Checks[key] = check
					}
				}
			}
			draft = next
		}
	} else {
		if !current || observation.Session != draft.ID {
			return out, fmt.Errorf("backup qualification session expired or installation/evidence changed; begin qualify again")
		}
		if action == "observe" {
			known := false
			for _, name := range workerQualificationChecks(c) {
				known = known || observation.ID == name
			}
			if draft.Recorded || !known || observation.Scope != "backup" || (observation.Result != "passed" && observation.Result != "failed") || !safefs.DigestPattern.MatchString(observation.EvidenceHash) {
				return out, fmt.Errorf("observation requires a pending check and evidence digest")
			}
			old, ok := draft.Checks[observation.ID]
			if ok && (old.Result != observation.Result || old.EvidenceHash != observation.EvidenceHash) {
				return out, fmt.Errorf("observation already retained; record this session before supplying new evidence")
			}
			if !ok {
				draft.Checks[observation.ID] = backupQualificationObservation{observation.Result, observation.EvidenceHash, now}
			}
		}
		if action == "commit" && !draft.Recorded {
			draft.Recorded = true
			if err = storeBackupEvaluationMeta(tx, "qualification", backupDraftProof(draft, c)); err != nil {
				return out, err
			}
			if err = storeBackupEvaluationMeta(tx, "qualification_binding", backupQualificationBindingRecord{1, draft.Binding, notify.Hash(backupDraftProof(draft, c))}); err != nil {
				return out, err
			}
		}
	}
	// Session and effective proof are committed atomically. A lost response can
	// replay the exact commit but cannot renew observations or retarget evidence.
	if err = storeBackupEvaluationMeta(tx, "qualification_session:"+draft.ID, draft); err != nil {
		return out, err
	}
	if err = storeBackupEvaluationMeta(tx, "qualification_draft", draft); err != nil {
		return out, err
	}
	out = install.QualificationSession{ID: draft.ID, CreatedAt: draft.CreatedAt, Recorded: draft.Recorded, EvidenceComplete: backupDraftComplete(draft, c)}
	for _, name := range workerQualificationChecks(c) {
		result := draft.Checks[name].Result
		if result == "" {
			result = "pending"
		}
		out.Checks = append(out.Checks, install.QualificationCheckSummary{Scope: "backup", ID: name, Result: result})
	}
	return out, nil
}

func backupDraftProof(d backupQualificationDraft, c WorkerInstallation) WorkerQualification {
	p := WorkerQualification{Schema: 1, WorkerID: c.WorkerID, ArchiveID: c.ArchiveID, ConfigHash: c.ConfigHash, Architecture: runtime.GOARCH, WorkerImage: c.Image, CheckedAt: d.CreatedAt, Checks: map[string]QualificationCheck{}}
	for name, check := range d.Checks {
		p.Checks[name] = QualificationCheck{Passed: check.Result == "passed", Evidence: "sha256:" + check.EvidenceHash}
	}
	return p
}
func backupDraftComplete(d backupQualificationDraft, c WorkerInstallation) bool {
	for _, name := range workerQualificationChecks(c) {
		if d.Checks[name].Result != "passed" {
			return false
		}
	}
	return true
}

func currentBackupQualification(proof WorkerQualification, c WorkerInstallation, now time.Time) bool {
	if !backupQualificationMatches(proof, c, now) {
		return false
	}
	for _, name := range workerQualificationChecks(c) {
		check := proof.Checks[name]
		if !check.Passed || len(check.Evidence) < 8 || len(check.Evidence) > 4096 {
			return false
		}
	}
	return true
}

func backupQualificationMatches(proof WorkerQualification, c WorkerInstallation, now time.Time) bool {
	if proof.Schema != 1 || proof.WorkerID != c.WorkerID || proof.ArchiveID != c.ArchiveID || proof.ConfigHash != c.ConfigHash || proof.WorkerImage != c.Image || proof.Architecture != runtime.GOARCH || proof.CheckedAt.IsZero() || now.Before(proof.CheckedAt) || now.Sub(proof.CheckedAt) > 7*24*time.Hour {
		return false
	}
	return true
}
