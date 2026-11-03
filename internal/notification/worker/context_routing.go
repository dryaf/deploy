package worker

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

const contextRetention = 90 * 24 * time.Hour

// Alias tombstones survive configuration changes. Reassignment requires an
// explicit administrative migration; merely editing the configuration is unsafe.
func (s *Store) reserveAliases(tx *sql.Tx) error {
	if s.config.Interaction == nil {
		return nil
	}
	for _, w := range s.config.Interaction.Workers {
		var id, epoch string
		err := tx.QueryRow("SELECT producer,epoch FROM command_aliases WHERE alias=?", w.Alias).Scan(&id, &epoch)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && (id != w.Producer.ID || epoch != w.Producer.Epoch) {
			return fmt.Errorf("worker alias is reserved; explicit reassignment required")
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO command_aliases VALUES(?,?,?)", w.Alias, w.Producer.ID, w.Producer.Epoch); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) freezeReference(tx *sql.Tx, m protocol.Message) error {
	if m.Context == nil || s.config.Interaction == nil {
		return nil
	}
	if err := s.reserveAliases(tx); err != nil {
		return err
	}
	for _, w := range s.config.Interaction.Workers {
		if w.Producer != m.Producer {
			continue
		}
		body, _ := json.Marshal(m.Context)
		_, err := tx.Exec("INSERT OR IGNORE INTO notification_references VALUES(?,?,?,?,?)", protocol.NotificationReference(m), m.Producer.ID, m.Producer.Epoch, w.Generation, string(body))
		return err
	}
	return nil
}

// Record acceptance even when the message became obsolete while sending. A
// conflicting native identity is quarantined, never reassigned to newer context.
func (s *Store) acceptReference(tx *sql.Tx, m protocol.Message, b *interaction.DeliveryBinding) error {
	if m.Context == nil || b == nil {
		return nil
	}
	d := b.Destination
	n, err := strconv.ParseInt(b.Native.ID, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != b.Native.ID || b.Native.Sender != d.Account || !protocol.ValidToken(d.ID) || !protocol.ValidToken(d.Generation) || !protocol.ValidToken(d.Account) || (d.Provider != "signal" && d.Provider != "telegram") {
		return nil
	}
	ref := protocol.NotificationReference(m)
	var exists int
	if err = tx.QueryRow("SELECT count(*) FROM notification_references WHERE reference=?", ref).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	_, err = tx.Exec(`INSERT INTO notification_deliveries(destination,native_id,sender,reference,expires) VALUES(?,?,?,?,?)
ON CONFLICT(destination,native_id,sender) DO UPDATE SET
conflict=CASE WHEN reference=excluded.reference THEN conflict ELSE 1 END,
expires=max(expires,excluded.expires)`, d.Fingerprint(), b.Native.ID, b.Native.Sender, ref, s.now().Add(contextRetention).UnixNano())
	return err
}

type referenceContext struct {
	worker    interaction.Worker
	context   protocol.InteractionContext
	reference string
}

func (s *Store) referenceContext(tx *sql.Tx, in interaction.Inbound, ref string) (out referenceContext, err error) {
	fail := func() (referenceContext, error) {
		return referenceContext{}, interaction.Reject("notification reference unavailable; use explicit worker and target")
	}
	c := s.config.Interaction
	known := false
	for _, p := range c.Permissions {
		if p.Actor == in.Actor && p.Conversation == in.Conversation {
			known = true
		}
	}
	if !known {
		return fail()
	}
	if in.Reply != nil {
		if !protocol.ValidToken(in.Reply.ID) || in.Reply.Sender != in.Account {
			return fail()
		}
		var native string
		err = tx.QueryRow("SELECT reference FROM notification_deliveries WHERE destination=? AND native_id=? AND sender=? AND expires>? AND conflict=0", in.DestinationFingerprint, in.Reply.ID, in.Reply.Sender, s.now().UnixNano()).Scan(&native)
		if err == sql.ErrNoRows {
			return fail()
		}
		if err != nil {
			return out, err
		}
		if ref != "" && ref != native {
			return fail()
		}
		ref = native
	}
	if !strings.HasPrefix(ref, "N-") || !protocol.ValidToken(ref) {
		return fail()
	}
	var producer protocol.Producer
	var generation, body string
	err = tx.QueryRow(`SELECT producer,epoch,generation,body FROM notification_references r WHERE reference=? AND EXISTS
(SELECT 1 FROM notification_deliveries d WHERE d.reference=r.reference AND destination=? AND expires>? AND conflict=0)`, ref, in.DestinationFingerprint, s.now().UnixNano()).Scan(&producer.ID, &producer.Epoch, &generation, &body)
	if err == sql.ErrNoRows {
		return fail()
	}
	if err != nil {
		return out, err
	}
	if err = protocol.Decode([]byte(body), &out.context, protocol.MaxRequest); err != nil {
		return out, err
	}
	if out.context.Validate() != nil {
		return fail()
	}
	for _, w := range c.Workers {
		if w.Producer == producer && w.Generation == generation {
			enrollment, e := s.config.enrollment(producer)
			if e != nil || !enrollment.InteractiveContext {
				return fail()
			}
			for _, target := range out.context.Targets {
				if !slices.Contains(enrollment.ContextKinds, target.Kind) {
					return fail()
				}
			}
			out.worker = w
			out.reference = ref
			return out, nil
		}
	}
	return fail()
}

// Resolve before validating domain arguments, so a native single-target reply
// can fill typed identifiers. The original input/hash remains unchanged.
func (s *Store) routeCommand(tx *sql.Tx, in interaction.Inbound) (parsed interaction.Parsed, bound *interaction.BoundContext, err error) {
	var resolved referenceContext
	text := in.Text
	if in.Reply != nil {
		resolved, err = s.referenceContext(tx, in, "")
		if err != nil {
			return
		}
		fields := strings.Fields(text)
		if len(fields) > 0 {
			if _, explicit := s.config.Interaction.Resolve(fields[0]); !explicit {
				if _, implicit := resolved.worker.Commands[fields[0]]; implicit {
					text = resolved.worker.Alias + " " + text
				}
			}
		}
	}
	parsed, err = interaction.Parse(text)
	if err != nil {
		err = interaction.Reject(err.Error() + "; send help for supported syntax")
		return
	}
	if ref := parsed.Arguments["ref"]; ref != "" {
		resolved, err = s.referenceContext(tx, in, ref)
		if err != nil {
			return
		}
		delete(parsed.Arguments, "ref")
	}
	if resolved.reference == "" {
		return
	}
	if parsed.Worker != resolved.worker.Alias {
		return parsed, nil, interaction.Reject("worker and notification context disagree")
	}
	d, ok := resolved.worker.Commands[parsed.Verb]
	if !ok {
		return parsed, nil, interaction.Reject("command unavailable")
	}
	scope, ok := s.config.Interaction.Authorize(in.Actor, in.Conversation, resolved.worker.Producer.ID, d.Permission)
	if !ok {
		return parsed, nil, interaction.Reject("command not authorized")
	}
	targetID, subject := parsed.Arguments["target"], parsed.Arguments["subject"]
	if resolved.context.Kind == "summary" && d.TargetRequired && targetID == "" {
		return parsed, nil, interaction.Reject("summary requires an explicit target")
	}
	var targets []protocol.Target
	for _, t := range resolved.context.Targets {
		if scope.Contains(t.Subject) && (targetID == "" || targetID == t.ID) && (subject == "" || subject == t.Subject) {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 || ((targetID != "" || d.TargetRequired) && len(targets) != 1) {
		return parsed, nil, interaction.Reject("target unavailable or ambiguous")
	}
	if len(targets) == 1 && (resolved.context.Kind == "single" || targetID != "") {
		if _, ok := d.Arguments["subject"]; ok {
			parsed.Arguments["subject"] = targets[0].Subject
		}
		if _, ok := d.Arguments["target"]; ok {
			parsed.Arguments["target"] = targets[0].ID
		}
		values := map[string]string{"occurrence": targets[0].Occurrence, "evidence_revision": strconv.FormatUint(targets[0].EvidenceRevision, 10), "decision_revision": strconv.FormatUint(targets[0].DecisionRevision, 10)}
		for key, value := range values {
			if a, ok := d.Arguments[key]; ok && a.Kind == "token" {
				if supplied := parsed.Arguments[key]; supplied != "" && supplied != value {
					return parsed, nil, interaction.Reject("explicit revision and notification context disagree")
				}
				parsed.Arguments[key] = value
			}
		}
	}
	resolved.context.Targets = targets
	bound = &interaction.BoundContext{Reference: resolved.reference, Context: resolved.context}
	return
}

func contextScope(bound *interaction.BoundContext) interaction.Scope {
	var ids []string
	for _, t := range bound.Context.Targets {
		ids = append(ids, t.Subject)
	}
	slices.Sort(ids)
	return interaction.Scope{Subjects: slices.Compact(ids)}
}

func (s *Store) confirmationReply(tx *sql.Tx, in interaction.Inbound, c interaction.Command) error {
	if in.Reply == nil {
		return nil
	}
	rejected := interaction.Reject("confirmation reply context disagrees or is unavailable")
	if !protocol.ValidToken(in.Reply.ID) || in.Reply.Sender != in.Account {
		return rejected
	}
	found := false
	var ref string
	var conflict int
	err := tx.QueryRow("SELECT reference,conflict FROM notification_deliveries WHERE destination=? AND native_id=? AND sender=?", in.DestinationFingerprint, in.Reply.ID, in.Reply.Sender).Scan(&ref, &conflict)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if conflict != 0 || c.Context == nil || c.Context.Reference != ref {
			return rejected
		}
		found = true
	}
	rows, err := tx.Query("SELECT command_id,body FROM command_reply_attempts WHERE native_id=? AND state='provider_accepted' LIMIT 257", in.Reply.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > 256 {
			return rejected
		}
		var id, body string
		var reply CommandReply
		if err = rows.Scan(&id, &body); err != nil {
			return err
		}
		if err = protocol.Decode([]byte(body), &reply, protocol.MaxRequest); err != nil {
			return err
		}
		if reply.Destination.Fingerprint() != in.DestinationFingerprint {
			continue
		}
		if id != c.ID {
			return rejected
		}
		found = true
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if !found {
		return rejected
	}
	return nil
}
