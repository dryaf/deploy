package backup

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	backupprotocol "github.com/dryaf/deploy/internal/backup/protocol"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

// This integration is deliberately not selected by a runtime configuration flag.
// A managed ownership handoff must precede production use. Installation holds the
// existing schema/notifier leases; the pump never owns messenger credentials.
type backupNotificationBinding struct {
	Identity          notify.Identity
	Producer          notify.Producer
	Archive           string
	Epoch             uint64
	Generation, Route string
}

func installBackupNotificationPipeline(tx *sql.Tx, b backupNotificationBinding) error {
	control, err := incidentControl(tx)
	if err != nil {
		return err
	}
	if control.WriterID != b.Producer.ID || control.ArchiveID != b.Archive || control.Epoch != b.Epoch || b.Archive == "" || b.Epoch == 0 || !notify.ValidToken(b.Generation) || !notify.ValidToken(b.Route) {
		return fmt.Errorf("backup notification authority mismatch")
	}
	installed, err := backupCommandsInstalled(tx)
	if err != nil {
		return err
	}
	if !installed {
		return fmt.Errorf("backup command domain must be initialized before interactive exports")
	}
	if err = installNotificationProducer(tx, b.Identity, b.Producer); err != nil {
		return err
	}
	_, err = tx.Exec("CREATE TABLE IF NOT EXISTS backup_command_event_context(event TEXT PRIMARY KEY,body TEXT NOT NULL);" +
		"CREATE TABLE IF NOT EXISTS backup_notification_binding(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL);" +
		"CREATE TABLE IF NOT EXISTS backup_notification_snapshots(sequence INTEGER PRIMARY KEY,basis INTEGER NOT NULL,body TEXT NOT NULL,report TEXT NOT NULL);" +
		"CREATE TABLE IF NOT EXISTS backup_notification_sources(source TEXT NOT NULL,event TEXT NOT NULL UNIQUE,revision INTEGER NOT NULL,PRIMARY KEY(source,revision));" +
		"CREATE TABLE IF NOT EXISTS backup_notification_members(obligation TEXT PRIMARY KEY,source TEXT NOT NULL,kind TEXT NOT NULL,incident TEXT NOT NULL);" +
		"CREATE TABLE IF NOT EXISTS backup_notification_transfers(event TEXT PRIMARY KEY,submitted INTEGER NOT NULL DEFAULT 0,cancel_reason TEXT NOT NULL DEFAULT '',cancel_sent INTEGER NOT NULL DEFAULT 0);" +
		"CREATE TABLE IF NOT EXISTS backup_notification_incidents(id TEXT PRIMARY KEY,hash TEXT NOT NULL,revision INTEGER NOT NULL);")
	if err != nil {
		return err
	}
	body, _ := json.Marshal(b)
	var old string
	err = tx.QueryRow("SELECT body FROM backup_notification_binding WHERE singleton=1").Scan(&old)
	if err == nil {
		if old != string(body) {
			return fmt.Errorf("notification pipeline binding changed; reconciliation required")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = tx.Exec("INSERT INTO backup_notification_binding VALUES(1,?)", string(body))
	return err
}
func readBackupNotificationBinding(tx *sql.Tx) (b backupNotificationBinding, err error) {
	var body string
	if err = tx.QueryRow("SELECT body FROM backup_notification_binding WHERE singleton=1").Scan(&body); err != nil {
		return
	}
	if err = notify.Decode([]byte(body), &b, 8192); err != nil {
		return
	}
	control, err := incidentControl(tx)
	if err != nil {
		return b, err
	}
	if control.WriterID != b.Producer.ID || control.ArchiveID != b.Archive || control.Epoch != b.Epoch {
		return b, fmt.Errorf("backup notification authority changed")
	}
	return b, nil
}

type backupNotificationEvaluation struct {
	Snapshot   notify.Snapshot
	ReportText string
}
type backupNotificationSource struct {
	event   notification.NotificationEvent
	created time.Time
}

// The caller evaluates domain state in this same transaction. Heartbeat sequence
// advances independently; basis changes only when the claims change. This is not
// an evaluator and must not turn an old textual report into current evidence.
func storeBackupNotificationSnapshot(tx *sql.Tx, b backupNotificationBinding, e backupNotificationEvaluation) error {
	var err error
	snap := e.Snapshot
	if err = snap.Validate(); err != nil {
		return err
	}
	if snap.Producer != b.Producer || strings.TrimSpace(e.ReportText) == "" {
		return fmt.Errorf("missing matching backup evaluation")
	}
	var previous, basis uint64
	if err = tx.QueryRow("SELECT COALESCE(max(sequence),0),COALESCE(max(basis),0) FROM backup_notification_snapshots").Scan(&previous, &basis); err != nil {
		return err
	}
	raw, _ := json.Marshal(snap)
	if snap.Sequence < previous || snap.BasisRevision < basis {
		return fmt.Errorf("backup evaluation regressed")
	}
	if snap.Sequence == previous {
		var old, report string
		if err = tx.QueryRow("SELECT body,report FROM backup_notification_snapshots WHERE sequence=?", previous).Scan(&old, &report); err != nil {
			return err
		}
		if old != string(raw) || report != e.ReportText {
			return fmt.Errorf("backup evaluation identity conflict")
		}
	} else {
		if _, err = tx.Exec("INSERT INTO backup_notification_snapshots VALUES(?,?,?,?)", snap.Sequence, snap.BasisRevision, string(raw), e.ReportText); err != nil {
			return err
		}
	}
	return nil
}

func freezeBackupNotificationPass(tx *sql.Tx, e backupNotificationEvaluation) ([]notify.Message, error) {
	if err := requireNotificationRecoveryInactive(tx); err != nil {
		return nil, err
	}
	b, err := readBackupNotificationBinding(tx)
	if err != nil {
		return nil, err
	}
	if err = storeBackupNotificationSnapshot(tx, b, e); err != nil {
		return nil, err
	}
	snap := e.Snapshot
	if err = registerBackupDailyObligations(tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query("SELECT record,created FROM outbox b WHERE "+backupPendingSourcePredicate+` AND due<=?
 AND NOT EXISTS (SELECT 1 FROM backup_notification_sources s JOIN notification_exports e ON e.id=s.event WHERE s.source=b.id AND e.state NOT IN ('expired','superseded','cancelled','provider_accepted'))
 ORDER BY created,id LIMIT 32`, snap.EvaluatedAt.UTC().Format(eventTimeLayout))
	if err != nil {
		return nil, err
	}
	var sources []backupNotificationSource
	for rows.Next() {
		var raw, created string
		var source backupNotificationSource
		if err = rows.Scan(&raw, &created); err != nil {
			rows.Close()
			return nil, err
		}
		if err = backupprotocol.StrictBackupJSON([]byte(raw), &source.event); err != nil {
			rows.Close()
			return nil, err
		}
		source.created, err = time.Parse(eventTimeLayout, created)
		if err != nil {
			rows.Close()
			return nil, err
		}
		sources = append(sources, source)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var exports []notify.Message
	for _, source := range sources {
		m, ok, err := freezeBackupSource(tx, b, e, source)
		if err != nil {
			return nil, err
		}
		if ok {
			exports = append(exports, m)
		}
	}
	return exports, nil
}

func backupIncidentReference(tx *sql.Tx, id string) (notify.Incident, error) {
	var subject, class, opened, message string
	var resolved sql.NullString
	if err := tx.QueryRow("SELECT instance,class,opened,message,resolved FROM incidents WHERE id=?", id).Scan(&subject, &class, &opened, &message, &resolved); err != nil {
		return notify.Incident{}, err
	}
	hash := notify.Hash([]any{subject, class, opened, message, resolved})
	var old string
	var revision uint64
	err := tx.QueryRow("SELECT hash,revision FROM backup_notification_incidents WHERE id=?", id).Scan(&old, &revision)
	if err != nil && err != sql.ErrNoRows {
		return notify.Incident{}, err
	}
	if old != hash {
		if !notify.Sequence(revision + 1) {
			return notify.Incident{}, fmt.Errorf("incident revision exhausted")
		}
		revision++
		if _, err = tx.Exec("INSERT INTO backup_notification_incidents VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET hash=excluded.hash,revision=excluded.revision", id, hash, revision); err != nil {
			return notify.Incident{}, err
		}
	}
	return notify.Incident{Subject: subject, ID: id, Revision: revision}, nil
}

func backupSourceMembers(tx *sql.Tx, event notification.NotificationEvent) ([]notification.NotificationEvent, error) {
	if event.Kind != "batch" {
		return []notification.NotificationEvent{event}, nil
	}
	if len(event.Members) == 0 || len(event.Members) > 128 {
		return nil, fmt.Errorf("invalid backup batch")
	}
	var members []notification.NotificationEvent
	seen := map[string]bool{}
	for _, id := range event.Members {
		if seen[id] {
			return nil, fmt.Errorf("duplicate backup member")
		}
		seen[id] = true
		var raw string
		var accepted sql.NullString
		if err := tx.QueryRow("SELECT record,accepted FROM outbox WHERE id=? AND covered_by=?", id, event.ID).Scan(&raw, &accepted); err != nil {
			return nil, err
		}
		if accepted.Valid {
			continue
		}
		var member notification.NotificationEvent
		if err := backupprotocol.StrictBackupJSON([]byte(raw), &member); err != nil {
			return nil, err
		}
		if member.Kind != "failure" && member.Kind != "reminder" && member.Kind != "recovery" && member.Kind != "retirement" && member.Kind != "test" {
			return nil, fmt.Errorf("batch contains non-alert obligation")
		}
		members = append(members, member)
	}
	return members, nil
}

func freezeBackupSource(tx *sql.Tx, b backupNotificationBinding, e backupNotificationEvaluation, source backupNotificationSource) (m notify.Message, ok bool, err error) {
	event := source.event
	members, err := backupSourceMembers(tx, event)
	if err != nil {
		return m, false, err
	}
	if event.Kind == "delivery-catchup" {
		if !strings.HasPrefix(event.ID, "catchup/") || !notify.ValidToken(strings.TrimPrefix(event.ID, "catchup/")) {
			return m, false, fmt.Errorf("invalid catchup outage")
		}
		var ready bool
		members, ready, err = backupCatchupMembers(tx, event)
		if err != nil || !ready {
			return m, false, err
		}
	}
	var obligations []notificationObligation
	refs := map[string]notify.Incident{}
	for _, member := range members {
		obligation := notificationObligation{ID: member.ID, Kind: "alert"}
		switch member.Kind {
		case "daily":
			obligation.Kind = "daily"
		case "delivery-catchup":
			obligation.Kind = "catchup"
			obligation.ID = strings.TrimPrefix(member.ID, "catchup/")
			if obligation.ID == member.ID {
				return m, false, fmt.Errorf("catchup has no outage identity")
			}
		case "test":
		case "failure", "reminder", "recovery", "retirement":
			ref, e := backupIncidentReference(tx, member.IncidentID)
			if e != nil {
				return m, false, e
			}
			refs[notify.Hash([]string{ref.Subject, ref.ID})] = ref
		default:
			return m, false, fmt.Errorf("unsupported backup source event")
		}
		var disposition, representative string
		err = tx.QueryRow("SELECT disposition,representative FROM notification_obligations WHERE id=?", obligation.ID).Scan(&disposition, &representative)
		if err != nil && err != sql.ErrNoRows {
			return m, false, err
		}
		if err == nil {
			if disposition != "open" {
				continue
			}
			if representative != "" {
				var state string
				if err = tx.QueryRow("SELECT state FROM notification_exports WHERE id=?", representative).Scan(&state); err != nil {
					return m, false, err
				}
				if state != "expired" && state != "superseded" && state != "cancelled" && state != "provider_accepted" {
					return m, false, nil
				}
			}
		}
		if _, err = tx.Exec("INSERT INTO backup_notification_members VALUES(?,?,?,?) ON CONFLICT(obligation) DO NOTHING", obligation.ID, member.ID, member.Kind, member.IncidentID); err != nil {
			return m, false, err
		}
		obligations = append(obligations, obligation)
	}
	if len(obligations) == 0 {
		return m, false, nil
	}
	var revision uint64
	if err = tx.QueryRow("SELECT COALESCE(max(revision),0) FROM backup_notification_sources WHERE source=?", event.ID).Scan(&revision); err != nil {
		return m, false, err
	}
	if !notify.Sequence(revision + 1) {
		return m, false, fmt.Errorf("report revision exhausted")
	}
	revision++
	now := e.Snapshot.EvaluatedAt
	m = notify.Message{Producer: b.Producer, ID: "backup-export/" + notify.Hash([]any{event.ID, revision}), Generation: b.Generation, Subject: "backup", Kind: event.Kind, Severity: "critical", ObservedAt: now, ExpiresAt: now.Add(5 * time.Minute), Text: notification.NotificationEnvelope(event.Message), Route: b.Route}
	if event.Kind == "failure" {
		m.ExpiresAt = time.Time{}
		m.ObservedAt = source.created
	}
	var keys []string
	for key := range refs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		m.Incidents = append(m.Incidents, refs[key])
	}
	switch event.Kind {
	case "test":
		m.Severity = "info"
	case "failure", "reminder", "recovery":
		if len(m.Incidents) != 1 {
			return m, false, fmt.Errorf("incident event missing target")
		}
		m.Subject = m.Incidents[0].Subject
		if event.Kind == "recovery" {
			m.Severity = "info"
			m.BasisRevision = e.Snapshot.BasisRevision
		}
	case "batch":
		m.Kind = "report"
		m.ReportKey = event.ID
		m.ReportType = "backlog"
		if len(m.Incidents) == 0 {
			m.ReportType = "diagnostic-backlog"
		}
		m.ReportRevision = revision
		m.BasisRevision = e.Snapshot.BasisRevision
		m.Text = notification.NotificationEnvelope("Current backup evaluation:\n" + e.ReportText + "\nHistorical backup alert backlog:\n" + event.Message)
	case "retirement":
		m.Kind = "report"
		m.Severity = "info"
		m.ReportKey = event.ID
		m.ReportType = "retirement"
		m.ReportRevision = revision
		m.BasisRevision = e.Snapshot.BasisRevision
		m.Text = notification.NotificationEnvelope("Current backup evaluation:\n" + e.ReportText + "\n" + event.Message)
	case "daily", "delivery-catchup":
		m.Kind = "report"
		m.Severity = "info"
		m.ReportKey = event.ID
		m.ReportType = "daily"
		m.ReportRevision = revision
		m.BasisRevision = e.Snapshot.BasisRevision
		if event.Kind == "delivery-catchup" {
			m.Kind = "catchup"
			m.ReportType = "delivery-catchup"
			m.OutageID = strings.TrimPrefix(event.ID, "catchup/")
		}
		m.Text = notification.NotificationEnvelope("Current backup evaluation for " + event.ID + ".\n" + e.ReportText)
		if event.Kind == "delivery-catchup" {
			final := false
			periods := 0
			for _, o := range obligations {
				if o.Kind == "catchup" {
					final = true
				}
				if o.Kind == "daily" {
					periods++
				}
			}
			claim := "Partial delivery-history catch-up; the delivery outage remains open."
			if final {
				claim = "Final delivery-history catch-up."
			}
			m.Text = notification.NotificationEnvelope(fmt.Sprintf("%s Outage %s; covers %d older daily periods.\n%s", claim, m.OutageID, periods, e.ReportText))
		}
	}
	m.Context, err = backupEventContext(tx, event.ID)
	if err != nil {
		return m, false, err
	}
	if m.Context != nil {
		copy := *m.Context
		copy.ID = "backup-export-context/" + notify.Hash(m.ID)
		m.Context = &copy
	}
	m, err = freezeNotificationExport(tx, m, obligations)
	if err != nil {
		return m, false, err
	}
	if _, err = tx.Exec("INSERT INTO backup_notification_sources VALUES(?,?,?)", event.ID, m.ID, revision); err != nil {
		return m, false, err
	}
	_, err = tx.Exec("INSERT INTO backup_notification_transfers(event) VALUES(?)", m.ID)
	return m, err == nil, err
}
