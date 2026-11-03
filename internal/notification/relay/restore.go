package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Restore stages evidence only. The live inbox (including any damaged database
// and sidecars) is preserved. No API here installs the candidate or enables intake.
type InboxRestoreState struct {
	Version                        int
	Operation, Account, Phase      string
	Before, After                  protocol.Identity
	Snapshot                       InboxSnapshotManifest
	CandidateHash                  string
	CandidateBytes                 int64
	IntakeEnabled, HistoryComplete bool
	GatewayExport                  string
	GatewaySummary                 string
	GatewayHandoff                 string
	GatewayCursor                  string
	BeforeBaseline                 string `json:",omitempty"`
}
type inboxRestoreBinding struct {
	Operation, Account, Snapshot string
	Before, After                protocol.Identity
	BeforeBaseline               string `json:",omitempty"`
}

func (j InboxRestoreState) binding() inboxRestoreBinding {
	return inboxRestoreBinding{j.Operation, j.Account, j.Snapshot.ID, j.Before, j.After, j.BeforeBaseline}
}
func (j InboxRestoreState) validate(account string) error {
	m := j.Snapshot
	if err := m.validate(account); err != nil {
		return err
	}
	if j.Version != 1 || j.IntakeEnabled || j.HistoryComplete || !safefs.ValidID(j.Operation, "n") || j.Account != account || !j.Before.Valid() || !j.After.Valid() || j.Before.Worker != j.After.Worker || j.Before.Recovery == j.After.Recovery || m.Identity != j.Before || (j.BeforeBaseline != "" && !safefs.DigestPattern.MatchString(j.BeforeBaseline)) {
		return fmt.Errorf("invalid relay restore authority")
	}
	switch j.Phase {
	case "prepared", "epoch_published":
		if j.CandidateHash != "" || j.CandidateBytes != 0 {
			return fmt.Errorf("premature relay restore completion")
		}
	case "quarantined":
		if !safefs.DigestPattern.MatchString(j.CandidateHash) || j.CandidateBytes <= 0 || j.CandidateBytes > inboxSnapshotBudget(j.Snapshot.Schema)+(1<<20) {
			return fmt.Errorf("invalid relay restore candidate")
		}
	default:
		return fmt.Errorf("unknown relay restore phase")
	}
	if j.GatewayExport != "" && (j.Phase != "quarantined" || !safefs.ValidID(j.GatewayExport, "n")) {
		return fmt.Errorf("invalid relay gateway export binding")
	}
	if j.GatewaySummary != "" && (j.GatewayExport == "" || !safefs.DigestPattern.MatchString(j.GatewaySummary)) {
		return fmt.Errorf("invalid relay gateway completion binding")
	}
	if j.GatewayHandoff != "" && (j.GatewaySummary == "" || !safefs.DigestPattern.MatchString(j.GatewayHandoff)) {
		return fmt.Errorf("invalid relay gateway handoff binding")
	}
	if j.GatewayCursor != "" && (j.GatewayHandoff == "" || !safefs.DigestPattern.MatchString(j.GatewayCursor)) {
		return fmt.Errorf("invalid relay gateway cursor binding")
	}
	return nil
}
func inboxRestoreBoundary(fault func(string) error, stage string) error {
	if fault != nil {
		return fault(stage)
	}
	return nil
}

func PrepareInboxRestore(ctx context.Context, path, account, snapshotPath string) (InboxRestoreState, error) {
	return prepareInboxRestore(ctx, path, account, snapshotPath, nil)
}
func prepareInboxRestore(ctx context.Context, path, account, snapshotPath string, fault func(string) error) (j InboxRestoreState, err error) {
	m, source, err := readInboxSnapshot(ctx, snapshotPath, account)
	if err != nil {
		return j, err
	}
	defer source.Close()
	dir, err := privateInboxDirectory(path)
	if err != nil {
		return j, err
	}
	defer dir.Close()
	lease, err := dir.Lock("inbox.lock", true, false)
	if err != nil {
		return j, err
	}
	defer lease.Close()
	if err = checkInboxRestoreFence(dir); err != nil {
		return
	}
	var pin inboxPin
	if err = dir.ReadJSON("identity.json", &pin); err != nil {
		return
	}
	if pin.Identity != m.Identity || pin.Account != account || pin.RecoveryOperation != "" || (pin.Baseline != "" && !safefs.DigestPattern.MatchString(pin.Baseline)) {
		return j, fmt.Errorf("snapshot belongs to another relay account or epoch")
	}
	if err = dir.Capacity(2*inboxSnapshotBudget(m.Schema) + (9 << 20)); err != nil {
		return
	}
	j = InboxRestoreState{Version: 1, Account: account, Before: pin.Identity, After: pin.Identity, Snapshot: m, Phase: "prepared", BeforeBaseline: pin.Baseline}
	if j.Operation, err = safefs.NewID("n"); err != nil {
		return
	}
	if j.After.Recovery, err = safefs.NewID("n"); err != nil {
		return
	}
	if err = dir.Mkdir("restore/" + j.Operation); err != nil {
		return
	}
	stage, err := dir.Sub("restore/" + j.Operation)
	if err != nil {
		return j, err
	}
	defer stage.Close()
	if err = copyInboxArtifact(source, "inbox.db", stage, "source.db", m); err != nil {
		return
	}
	if err = copyInboxArtifact(stage, "source.db", stage, "candidate.db", m); err != nil {
		return
	}
	if err = dir.WriteJSON("restore.json", j, false); err != nil {
		return
	}
	if err = inboxRestoreBoundary(fault, "restore_prepared"); err != nil {
		return
	}
	err = finishInboxRestore(ctx, dir, &j, fault)
	return
}
func ResumeInboxRestore(ctx context.Context, path, account string) (InboxRestoreState, error) {
	return resumeInboxRestore(ctx, path, account, nil)
}
func resumeInboxRestore(ctx context.Context, path, account string, fault func(string) error) (j InboxRestoreState, err error) {
	dir, err := privateInboxDirectory(path)
	if err != nil {
		return j, err
	}
	defer dir.Close()
	lease, err := dir.Lock("inbox.lock", true, false)
	if err != nil {
		return j, err
	}
	defer lease.Close()
	if err = dir.ReadJSON("restore.json", &j); err != nil {
		return
	}
	if err = j.validate(account); err != nil {
		return
	}
	err = finishInboxRestore(ctx, dir, &j, fault)
	return
}
func finishInboxRestore(ctx context.Context, dir *safefs.Directory, j *InboxRestoreState, fault func(string) error) error {
	if err := j.validate(j.Account); err != nil {
		return err
	}
	var pin inboxPin
	if err := dir.ReadJSON("identity.json", &pin); err != nil {
		return err
	}
	before := inboxPin{Identity: j.Before, Account: j.Account, Baseline: j.BeforeBaseline}
	after := inboxPin{Identity: j.After, Account: j.Account, RecoveryOperation: j.Operation}
	if pin != before && pin != after {
		return fmt.Errorf("relay identity changed outside restore")
	}
	if j.Phase != "prepared" && pin != after {
		return fmt.Errorf("relay restore epoch rolled back")
	}
	stage, err := dir.Sub("restore/" + j.Operation)
	if err != nil {
		return err
	}
	defer stage.Close()
	if err = verifyStandaloneInboxFile(stage, "source.db", j.Snapshot.DatabaseBytes, j.Snapshot.DatabaseHash); err != nil {
		return err
	}
	if j.Phase == "quarantined" {
		return verifyStandaloneInboxFile(stage, "candidate.db", j.CandidateBytes, j.CandidateHash)
	}
	if err = dir.WriteJSON("identity.json", after, true); err != nil {
		return err
	}
	if err = inboxRestoreBoundary(fault, "restore_identity"); err != nil {
		return err
	}
	j.Phase = "epoch_published"
	if err = dir.WriteJSON("restore.json", j, true); err != nil {
		return err
	}
	if err = quarantineInboxCandidate(ctx, stage, *j, fault); err != nil {
		return err
	}
	f, err := inboxArtifactFile(stage, "candidate.db", inboxSnapshotBudget(j.Snapshot.Schema)+(1<<20))
	if err != nil {
		return err
	}
	i, err := f.Stat()
	if err == nil {
		j.CandidateHash, err = safefs.DigestFile(f)
	}
	f.Close()
	if err != nil {
		return err
	}
	j.CandidateBytes = i.Size()
	if err = verifyStandaloneInboxFile(stage, "candidate.db", j.CandidateBytes, j.CandidateHash); err != nil {
		return err
	}
	j.Phase = "quarantined"
	if err = dir.WriteJSON("restore.json", j, true); err != nil {
		return err
	}
	return inboxRestoreBoundary(fault, "restore_quarantined")
}
func quarantineInboxCandidate(ctx context.Context, dir *safefs.Directory, j InboxRestoreState, fault func(string) error) error {
	f, err := inboxArtifactFile(dir, "candidate.db", inboxSnapshotBudget(j.Snapshot.Schema)+(1<<20))
	if err != nil {
		return err
	}
	f.Close()
	db, err := inboxArtifactDB(dir, "candidate.db", false)
	if err != nil {
		return err
	}
	defer db.Close()
	var check string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return fmt.Errorf("relay candidate integrity unavailable")
	}
	var present int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='restore_binding'").Scan(&present); err != nil {
		return err
	}
	if present != 0 {
		if err = verifyQuarantinedInbox(ctx, db, j); err != nil {
			return err
		}
		if err = db.Close(); err != nil {
			return err
		}
		return dir.File().Sync()
	}
	// The working candidate may retain a rollback journal from SIGKILL. SQLite
	// has recovered any hot journal above; an untouched candidate can also have a
	// cold journal until the next write. Its main file must still match exactly.
	if err = verifyInboxFile(dir, "candidate.db", j.Snapshot.DatabaseBytes, j.Snapshot.DatabaseHash); err != nil {
		return err
	}
	observed, err := inspectInboxArtifact(ctx, db)
	if err != nil {
		return err
	}
	if observed.Identity != j.Before || observed.Account != j.Account || observed.EventsDigest != j.Snapshot.EventsDigest {
		return fmt.Errorf("unbound relay candidate changed")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TABLE restore_identity AS SELECT * FROM identity;
CREATE TABLE restore_binding(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL);
`); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", inboxQuarantineSchema(j.Snapshot.Schema))); err != nil {
		return err
	}
	pin, _ := json.Marshal(inboxPin{Identity: j.After, Account: j.Account, RecoveryOperation: j.Operation})
	binding, _ := json.Marshal(j.binding())
	if _, err = tx.Exec("UPDATE identity SET body=?,acked=0,listed=0 WHERE singleton=1", string(pin)); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO restore_binding VALUES(1,?)", string(binding)); err != nil {
		return err
	}
	if err = inboxRestoreBoundary(fault, "restore_transaction"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = inboxRestoreBoundary(fault, "restore_commit"); err != nil {
		return err
	}
	if err = verifyQuarantinedInbox(ctx, db, j); err != nil {
		return err
	}
	if err = db.Close(); err != nil {
		return err
	}
	return dir.File().Sync()
}
func verifyQuarantinedInbox(ctx context.Context, db *sql.DB, j InboxRestoreState) error {
	if j.Snapshot.Schema == 2 {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name IN ('recovery_exports','recovery_records') AND type='table'").Scan(&count); err != nil || count != 2 {
			return fmt.Errorf("relay quarantine lost prior export schema")
		}
	}
	var version int
	var body string
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != inboxQuarantineSchema(j.Snapshot.Schema) {
		return fmt.Errorf("relay quarantine schema changed")
	}
	if err := db.QueryRowContext(ctx, "SELECT body FROM restore_binding WHERE singleton=1").Scan(&body); err != nil {
		return err
	}
	var binding inboxRestoreBinding
	if err := protocol.Decode([]byte(body), &binding, 4096); err != nil || binding != j.binding() {
		return fmt.Errorf("relay quarantine binding changed")
	}
	for _, old := range []bool{true, false} {
		table, expected, ack, listed := "identity", inboxPin{Identity: j.After, Account: j.Account, RecoveryOperation: j.Operation}, uint64(0), uint64(0)
		if old {
			table = "restore_identity"
			expected = inboxPin{Identity: j.Before, Account: j.Account, Baseline: j.Snapshot.BaselineHash}
			ack = j.Snapshot.Acknowledged
			listed = j.Snapshot.Listed
		}
		var actualAck, actualListed uint64
		if err := db.QueryRowContext(ctx, "SELECT body,acked,listed FROM "+table+" WHERE singleton=1").Scan(&body, &actualAck, &actualListed); err != nil {
			return err
		}
		var pin inboxPin
		if err := protocol.Decode([]byte(body), &pin, 4096); err != nil || pin != expected || actualAck != ack || actualListed != listed {
			return fmt.Errorf("relay quarantine identity or watermark changed")
		}
	}
	var highest uint64
	if err := db.QueryRowContext(ctx, "SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='events'),0)").Scan(&highest); err != nil {
		return err
	}
	if highest != j.Snapshot.Highest {
		return fmt.Errorf("relay quarantine sequence changed")
	}
	if j.Snapshot.Schema == runtimeInboxSchema {
		return verifyBaselineQuarantine(ctx, db, j)
	}
	hash, err := inboxEventDigest(ctx, db, j.Account, highest)
	if err != nil {
		return err
	}
	if hash != j.Snapshot.EventsDigest {
		return fmt.Errorf("relay quarantine event history changed")
	}
	return nil
}
