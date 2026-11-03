package worker

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

func TestManagedVerificationRefusesImplicitSchemaMigration(t *testing.T) {
	s, _, path := fixture(t)
	_, err := s.db.Exec("PRAGMA user_version=8")
	must(t, err)
	must(t, s.Close())
	if v, e := OpenManagedVerification(path, configuration(), s.Identity()); e == nil {
		v.Close()
		t.Fatal("managed open silently upgraded state")
	}
	db, err := sql.Open("sqlite", path+"/state.db")
	must(t, err)
	defer db.Close()
	var version int
	must(t, db.QueryRow("PRAGMA user_version").Scan(&version))
	if version != 8 {
		t.Fatal("rejected managed open modified schema", version)
	}
}

func TestManagedVerificationRejectsWrongIdentityBeforeAttemptRecovery(t *testing.T) {
	s, now, path := fixture(t)
	ctx := context.Background()
	m := report(*now, "managed-identity", 1, 1, 1)
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	j, granted, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !granted || j.Attempt == "" {
		t.Fatal("no in-flight fixture attempt")
	}
	id := s.Identity()
	id.Recovery = "wrong-recovery"
	must(t, s.Close())
	if v, e := OpenManagedVerification(path, configuration(), id); e == nil {
		v.Close()
		t.Fatal("wrong identity accepted")
	}
	if v, e := OpenManagedVerification(path, configuration(), protocol.Identity{}); e == nil {
		v.Close()
		t.Fatal("unpinned identity accepted")
	}
	db, err := sql.Open("sqlite", path+"/state.db")
	must(t, err)
	defer db.Close()
	var attempt, state string
	must(t, db.QueryRow("SELECT attempt,state FROM messages WHERE producer=? AND id=?", producer.ID, m.ID).Scan(&attempt, &state))
	if attempt != j.Attempt || state != "sending" {
		t.Fatal("rejected open recovered a foreign attempt", attempt, state)
	}
}

func TestVerificationModeKeepsDurableAdmissionWithoutGrants(t *testing.T) {
	s, now, path := fixture(t)
	ctx := context.Background()
	m := report(*now, "verify-report", 1, 1, 1)
	must(t, s.Close())
	v, err := OpenVerification(path, configuration())
	must(t, err)
	defer v.Close()
	v.now = func() time.Time { return *now }
	if !v.VerificationOnly() {
		t.Fatal("mode lost")
	}
	if err = v.Activate(producer, "generation-one"); err == nil {
		t.Fatal("verification activated producer")
	}
	_, err = v.Enqueue(ctx, m)
	must(t, err)
	must(t, v.Replace(ctx, snapshot(*now, 1, 1)))
	j, granted, err := v.Grant(ctx, producer, m.ID)
	must(t, err)
	if granted || j.Tries != 0 || j.Attempt != "" {
		t.Fatal("verification granted delivery", j)
	}
	if _, err = v.GrantCommand(ctx, producer, "anything"); err == nil {
		t.Fatal("verification granted execution")
	}
	if _, err = v.AdmitCommand(ctx, interaction.Inbound{}); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatal("verification admitted chat command", err)
	}
	if _, err = v.AdmitGatewayQuery(ctx, interaction.Inbound{}); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatal("verification admitted chat query", err)
	}
	_, granted, err = v.grantReply(ctx, CommandReply{})
	must(t, err)
	if granted {
		t.Fatal("verification granted reply")
	}
	if err = v.RunTelegramIntake(ctx, provider.TelegramPoller{}); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatal("verification entered legacy Telegram intake loop", err)
	}
	if err = v.PullTelegram(ctx, provider.TelegramPoller{}); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatal(err)
	}
	if err = v.PullSignal(ctx, provider.SignalInboxClient{}); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatal(err)
	}
	h, err := v.Health(ctx)
	must(t, err)
	if !h.VerificationOnly || h.Condition != "unknown" || !h.ValidUntil.IsZero() {
		t.Fatal(h)
	}
	must(t, v.Close())
	reopened, err := Open(path, configuration(), false)
	must(t, err)
	defer reopened.Close()
	reopened.now = func() time.Time { return *now }
	// Durable evidence survived, but startup freshness still needs a new status.
	must(t, reopened.Replace(ctx, snapshot(*now, 2, 1)))
	j, granted, err = reopened.Grant(ctx, producer, m.ID)
	must(t, err)
	if !granted || j.Tries != 1 {
		t.Fatal("verification lost or altered queued obligation", j)
	}
	must(t, reopened.Finish(ctx, j, Result{Accepted: true}))
}

func TestVerificationModeRevokesHealthyAndRetainsFaults(t *testing.T) {
	s, now, path := bindingFixture(t, binding("selected", "backup"))
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	var fault string
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		fault, err = s.currentLEDState(tx, "selected", "critical")
		return err
	}))
	api := &fakeLED{states: map[string]led.State{fault: {ID: fault, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true}}}
	for _, b := range []string{workerLEDBinding, "selected"} {
		id := ledBindingNamespace(s.Identity().Worker, b) + ".healthy"
		ttl := 20
		api.states[id] = led.State{ID: id, LED: "green", Pattern: "heartbeat", Priority: 10, TTL: &ttl}
	}
	c := s.config
	must(t, s.Close())
	v, err := OpenVerification(path, c)
	must(t, err)
	defer v.Close()
	v.now = func() time.Time { return *now }
	must(t, v.Replace(ctx, snapshot(*now, 2, 2)))
	r := LEDReconciler{Store: v, API: api, Budget: time.Second}
	for range 2 {
		must(t, r.Step(ctx))
	}
	if _, ok := api.states[fault]; !ok {
		t.Fatal("verification cleared fault with fresh healthy status")
	}
	for _, state := range api.states {
		if strings.HasSuffix(state.ID, ".healthy") || state.Pattern == "heartbeat" {
			t.Fatal("verification kept healthy output", state)
		}
	}
	var count int
	must(t, v.db.QueryRow("SELECT count(*) FROM led_states WHERE id=? AND obsolete=0", fault).Scan(&count))
	if count != 1 {
		t.Fatal("verification rewrote cleanup authority")
	}
	h, err := v.Health(ctx)
	must(t, err)
	if !h.VerificationOnly || h.Condition != "unknown" || !h.LEDOutputs["selected"].Suppressed || h.LEDOutputs["selected"].VisualVerified {
		t.Fatal(h)
	}
}
