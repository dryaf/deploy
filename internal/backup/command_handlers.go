package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

func backupCommandManifest(mutations bool) map[string]interaction.Descriptor {
	token := interaction.Argument{Kind: "token", Required: true, Max: 128}
	result := map[string]interaction.Descriptor{
		"status": {ID: "backup.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "Count open incidents within your authorized subjects; this does not qualify backup freshness."},
	}
	for _, name := range []string{"details", "why"} {
		result[name] = interaction.Descriptor{ID: "backup." + name, Version: 1, Permission: "read", Effect: "read", Scoped: true, TargetRequired: true, Arguments: map[string]interaction.Argument{"subject": token, "target": token}, Help: "Read an exact incident, its occurrence and evidence/decision revisions."}
	}
	if mutations {
		for _, name := range []string{"ack", "snooze", "unsnooze"} {
			args := map[string]interaction.Argument{"subject": token, "target": token, "evidence_revision": token, "decision_revision": token}
			if name == "snooze" {
				args["duration"] = interaction.Argument{Kind: "duration", Required: true, Max: 86400}
			}
			result[name] = interaction.Descriptor{ID: "backup." + name, Version: 1, Permission: "incident.manage", Effect: "mutation", Scoped: true, TargetRequired: true, Arguments: args, Help: "Change reminder suppression for one current incident occurrence; health and recovery reporting remain unchanged."}
		}
	}
	return result
}

func initializeBackupCommands(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE backup_command_suppressions(incident TEXT PRIMARY KEY,evidence INTEGER NOT NULL,revision INTEGER NOT NULL,created TEXT NOT NULL,expires TEXT NOT NULL,actor TEXT NOT NULL,command_id TEXT NOT NULL);
CREATE TABLE backup_command_audit(command_id TEXT PRIMARY KEY,incident TEXT NOT NULL,body TEXT NOT NULL);
CREATE TABLE backup_command_evidence(incident TEXT PRIMARY KEY,hash TEXT NOT NULL,revision INTEGER NOT NULL);
CREATE TABLE backup_command_event_context(event TEXT PRIMARY KEY,body TEXT NOT NULL);
CREATE TABLE backup_command_delivery_guard(singleton INTEGER PRIMARY KEY CHECK(singleton=1),active INTEGER NOT NULL CHECK(active IN (0,1)));
INSERT INTO backup_command_delivery_guard VALUES(1,0);`)
	if err != nil {
		return err
	}
	rows, err := tx.Query("SELECT id,class,opened,message FROM incidents")
	if err != nil {
		return err
	}
	type incident struct{ id, class, opened, message string }
	var records []incident
	for rows.Next() {
		var r incident
		if err = rows.Scan(&r.id, &r.class, &r.opened, &r.message); err != nil {
			rows.Close()
			return err
		}
		records = append(records, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range records {
		if _, err = syncBackupCommandEvidence(tx, r.id, r.class, r.opened, r.message); err != nil {
			return err
		}
	}
	return nil
}

type backupCommandHandler struct {
	name       string
	descriptor interaction.Descriptor
	worker     interaction.Worker
	policy     interaction.Config
	now        func() time.Time
}

func backupCommandHandlers(policy interaction.Config, p notify.Producer, mutations bool, now func() time.Time) (map[string]client.CommandHandler, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	var worker interaction.Worker
	for _, w := range policy.Workers {
		if w.Producer == p {
			worker = w
		}
	}
	if worker.Alias == "" {
		return nil, fmt.Errorf("backup command producer not enrolled")
	}
	approved := backupCommandManifest(mutations)
	handlers := map[string]client.CommandHandler{}
	for name, desc := range worker.Commands {
		if expected, ok := approved[name]; !ok || notify.Hash(desc) != notify.Hash(expected) {
			return nil, fmt.Errorf("backup command contract is not approved: %s", name)
		}
		handlers[desc.ID] = backupCommandHandler{name: name, descriptor: desc, worker: worker, policy: policy, now: now}
	}
	return handlers, nil
}

func (h backupCommandHandler) Descriptor() interaction.Descriptor { return h.descriptor }
func (h backupCommandHandler) Prepare(context.Context, interaction.Command) (interaction.Preparation, error) {
	return interaction.Preparation{}, fmt.Errorf("backup incident commands do not use confirmation")
}

func (h backupCommandHandler) Apply(tx *sql.Tx, c interaction.Command) (r interaction.Result, err error) {
	r.Scope = c.Scope
	r.State = "failed"
	reject := func(message string) (interaction.Result, error) { r.Text = message; return r, nil }
	allowed, ok := h.policy.Authorize(c.Actor, c.Destination.ID, h.worker.Producer.ID, h.descriptor.Permission)
	if !ok || !allowed.Allows(c.Scope) || c.Producer != h.worker.Producer || c.Generation != h.worker.Generation || notify.Hash(c.Descriptor) != notify.Hash(h.descriptor) {
		return reject("Command is no longer authorized by the backup worker.")
	}
	destination := false
	for _, d := range h.policy.Destinations {
		if d == c.Destination {
			destination = true
		}
	}
	if !destination {
		return reject("Command conversation is no longer enrolled.")
	}
	if err = interaction.ValidateArguments(h.descriptor, c.Arguments); err != nil {
		return reject("Invalid backup command arguments.")
	}
	if h.name == "status" {
		where, args := backupScopePredicate(c.Scope)
		var count int
		if err = tx.QueryRow("SELECT count(*) FROM incidents WHERE resolved IS NULL AND "+where, args...).Scan(&count); err != nil {
			return r, err
		}
		r.State = "succeeded"
		r.Text = fmt.Sprintf("Open incidents in authorized scope: %d. Incident state does not establish current backup freshness or application recovery.", count)
		return r, nil
	}
	subject, target := c.Arguments["subject"], c.Arguments["target"]
	if !c.Scope.Contains(subject) {
		return reject("Incident unavailable in the authorized scope.")
	}
	r.Scope = interaction.Scope{Subjects: []string{subject}}
	var class, opened, message string
	var resolved sql.NullString
	err = tx.QueryRow("SELECT class,opened,message,resolved FROM incidents WHERE id=? AND instance=?", target, subject).Scan(&class, &opened, &message, &resolved)
	if err == sql.ErrNoRows {
		return reject("Incident unavailable in the authorized scope.")
	}
	if err != nil {
		return r, err
	}
	var evidence uint64
	var evidenceHash string
	if err = tx.QueryRow("SELECT hash,revision FROM backup_command_evidence WHERE incident=?", target).Scan(&evidenceHash, &evidence); err != nil {
		return r, err
	}
	if evidenceHash != notify.Hash([]string{class, opened, message}) {
		return reject("Incident evidence needs reconciliation.")
	}
	revision := uint64(1)
	var savedEvidence uint64
	var created, expires string
	err = tx.QueryRow("SELECT evidence,revision,created,expires FROM backup_command_suppressions WHERE incident=?", target).Scan(&savedEvidence, &revision, &created, &expires)
	if err != nil && err != sql.ErrNoRows {
		return r, err
	}
	if c.Context != nil {
		if len(c.Context.Context.Targets) != 1 {
			return reject("Incident context is ambiguous.")
		}
		t := c.Context.Context.Targets[0]
		if t.Kind != "incident" || t.Subject != subject || t.ID != target || t.Occurrence != target {
			return reject("Incident context does not match this occurrence.")
		}
		if h.descriptor.Effect == "mutation" && (t.EvidenceRevision != evidence || t.DecisionRevision != revision) {
			return reject("Incident evidence or policy changed; request current details before changing reminders.")
		}
	}
	if h.descriptor.Effect == "read" {
		state := "open"
		if resolved.Valid {
			state = "resolved"
		}
		suppression := "none"
		suppressed, e := incidentReminderSuppressed(tx, target, evidence, h.now())
		if e != nil {
			return r, e
		}
		if suppressed {
			suppression = expires
		}
		message = boundedIncidentText(message, 1500)
		r.State = "succeeded"
		r.Text = fmt.Sprintf("Incident %s (%s), subject %s, class %s.\n%s\nOccurrence: %s; evidence_revision=%d decision_revision=%d. Reminder suppression: %s.", target, state, subject, class, message, target, evidence, revision, suppression)
		return r, nil
	}
	if resolved.Valid {
		return reject("Incident is already resolved; no reminder policy changed.")
	}
	// The legacy notifier sends outside SQLite. Until shared exporter reservations
	// replace it, reject suppression changes during a whole notifier dispatch pass.
	// This avoids splitting a mixed batch after a provider has started accepting it.
	var dispatching int
	if err = tx.QueryRow("SELECT active FROM backup_command_delivery_guard WHERE singleton=1").Scan(&dispatching); err != nil {
		return r, err
	}
	if dispatching != 0 {
		return reject("Legacy notification delivery is in progress; retry this reminder change after the dispatch pass.")
	}
	control, e := incidentControl(tx)
	if e != nil {
		return r, e
	}
	if retirementReason(control, subject) != "" {
		return reject("Incident subject is retired.")
	}
	var clock string
	err = tx.QueryRow("SELECT value FROM meta WHERE key='clock_suspect'").Scan(&clock)
	if err != nil && err != sql.ErrNoRows {
		return r, err
	}
	if err == nil && clock != "false" {
		return reject("Backup worker clock is uncertain.")
	}
	expectedEvidence, e := strconv.ParseUint(c.Arguments["evidence_revision"], 10, 63)
	if e != nil || expectedEvidence != evidence {
		return reject("Incident evidence changed; request current details.")
	}
	expectedDecision, e := strconv.ParseUint(c.Arguments["decision_revision"], 10, 63)
	if e != nil || expectedDecision != revision {
		return reject("Incident reminder policy changed; request current details.")
	}
	now := h.now().UTC()
	openedAt, e := time.Parse(eventTimeLayout, opened)
	if e != nil {
		return r, e
	}
	if now.Before(openedAt) {
		return reject("Backup worker clock is uncertain.")
	}
	if created != "" {
		then, e := time.Parse(eventTimeLayout, created)
		if e != nil {
			return r, e
		}
		if now.Before(then) {
			return reject("Backup worker clock precedes the last reminder decision.")
		}
	}
	duration := time.Hour
	if h.name == "snooze" {
		duration, e = interaction.Duration(c.Arguments["duration"])
		if e != nil || duration > 24*time.Hour {
			return reject("Snooze must be between one minute and 24 hours.")
		}
	}
	if h.name == "unsnooze" {
		duration = 0
	}
	if revision >= uint64(1<<63-1) {
		return reject("Incident decision revision exhausted.")
	}
	until := now.Add(duration)
	if _, err = tx.Exec(`INSERT INTO backup_command_suppressions VALUES(?,?,?,?,?,?,?) ON CONFLICT(incident) DO UPDATE SET evidence=excluded.evidence,revision=excluded.revision,created=excluded.created,expires=excluded.expires,actor=excluded.actor,command_id=excluded.command_id`, target, evidence, revision+1, now.Format(eventTimeLayout), until.Format(eventTimeLayout), c.Actor, c.ID); err != nil {
		return r, err
	}
	if duration > 0 {
		if err = cancelIncidentReminders(tx, target, c.ID); err != nil {
			return r, err
		}
	}
	audit, _ := json.Marshal(struct {
		Actor, Subject, Target, Action string
		Evidence, Revision             uint64
		At, Until                      time.Time
	}{c.Actor, subject, target, h.name, evidence, revision + 1, now, until})
	if _, err = tx.Exec("INSERT INTO backup_command_audit VALUES(?,?,?)", c.ID, target, string(audit)); err != nil {
		return r, err
	}
	r.State = "succeeded"
	r.Text = fmt.Sprintf("Incident %s: reminder suppression until %s. Decision revision %d. Incident health, recovery messages and daily reports are unchanged.", target, until.Format(time.RFC3339), revision+1)
	return r, nil
}

func backupScopePredicate(scope interaction.Scope) (string, []any) {
	if scope.WholeWorker {
		return "1=1", nil
	}
	var args []any
	for _, s := range scope.Subjects {
		args = append(args, s)
	}
	return "instance IN (" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")", args
}

func suppressionCurrent(created, expires string, now time.Time) bool {
	a, e := time.Parse(eventTimeLayout, created)
	if e != nil || now.Before(a) {
		return false
	}
	b, e := time.Parse(eventTimeLayout, expires)
	return e == nil && b.After(now)
}
func boundedIncidentText(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && !utf8.ValidString(text[:n]) {
		n--
	}
	return text[:n] + " [truncated]"
}
