package workerctl

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDeclaredBackupProducerPolicySurvivesUnboundEnrollment(t *testing.T) {
	r, paths := bootstrapControlFixture(t)
	required := false
	policy := config.ProducerDeclaration{Required: &required, Routes: []string{"alerts"}, MaxSilenceSeconds: 180, MaxEvidenceSeconds: 100000, MaxMessages: 321, PolicyHealth: true}
	r.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": policy}
	testutil.Must(t, directNotificationSupport(r.Desired))
	input, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs)
	testutil.Must(t, err)
	if !input.Bootstrap || len(input.Worker.Producers) != 0 {
		t.Fatal("notification-first setup fabricated a producer")
	}
	before, err := input.Resolve()
	testutil.Must(t, err)
	parent, err := ensureConfigDirectory(filepath.Dir(paths.State))
	testutil.Must(t, err)
	parent.Close()
	b, err := worker.BootstrapGateway(context.Background(), paths.State, testutil.ID(t, "n"), before.Worker)
	testutil.Must(t, err)
	p := protocol.Producer{ID: testutil.ID(t, "w"), Epoch: testutil.ID(t, "n")}
	compiled, err := CompileNotification(r.Desired, r.Facts, paths, NotificationBindings{Providers: input.Providers, Producers: map[string]ProducerBinding{"backup-worker": {Producer: p, Generation: testutil.ID(t, "g")}}})
	testutil.Must(t, err)
	next, err := compiled.Input.Resolve()
	testutil.Must(t, err)
	want := worker.Enrollment{Producer: p, Required: false, Routes: policy.Routes, MaxSilenceSeconds: 180, MaxEvidenceSeconds: 100000, MaxMessages: 321, PolicyHealth: true}
	if compiled.Input.Bootstrap || !reflect.DeepEqual(next.Worker.Producers, []worker.Enrollment{want}) {
		t.Fatal("explicit producer policy lost", next.Worker.Producers)
	}
	op := testutil.ID(t, "n")
	receipt, err := worker.EnrollBootstrap(context.Background(), paths.State, b.Identity, next.Worker, op)
	testutil.Must(t, err)
	store, err := worker.OpenManagedVerification(paths.State, next.Worker, b.Identity)
	testutil.Must(t, err)
	testutil.Must(t, store.RequireInactiveProducers())
	testutil.Must(t, store.Close())
	again, err := worker.EnrollBootstrap(context.Background(), paths.State, b.Identity, next.Worker, op)
	testutil.Must(t, err)
	if receipt != again || receipt.Identity != b.Identity {
		t.Fatal("explicit policy enrollment replaced identity or repeated revision")
	}
	// Default backup freshness can now change independently when the operator
	// has deliberately declared the producer's own evidence contract.
	hash, err := r.Desired.NotificationFingerprint()
	testutil.Must(t, err)
	r.Desired.Backup.Freshness = "26h"
	nextHash, err := r.Desired.NotificationFingerprint()
	testutil.Must(t, err)
	if hash != nextHash {
		t.Fatal("explicit producer bound was overwritten by backup defaults")
	}
}

func TestShortProducerPolicyRequiresItsRealDeclaredConsumer(t *testing.T) {
	r, _ := bootstrapControlFixture(t)
	policy := config.ProducerDeclaration{Routes: []string{"alerts"}}
	for _, kind := range []string{"unknown", "absent-backup", "disabled-backup", "wrong-route"} {
		t.Run(kind, func(t *testing.T) {
			d := r.Desired
			n, b := *d.Notification, *d.Backup
			d.Notification, d.Backup = &n, &b
			n.Producers = map[string]config.ProducerDeclaration{"backup-worker": policy}
			switch kind {
			case "unknown":
				n.Producers = map[string]config.ProducerDeclaration{"invented": policy}
			case "absent-backup":
				d.Backup = nil
			case "disabled-backup":
				disabled := false
				b.Enabled = &disabled
			case "wrong-route":
				b.Notifications.Route = "not-authorized"
			}
			if directNotificationSupport(d) == nil {
				t.Fatal("unsupported consumer accepted")
			}
		})
	}
}
