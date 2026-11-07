package workerctl

import (
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestFreshSignalDefersExplicitBackupProducerUntilReservation(t *testing.T) {
	for _, telegram := range []bool{false, true} {
		r, paths, review, _ := signalAdoptionFixture(t, telegram)
		r.Desired.Backup = &config.BackupDeclaration{Freshness: "26h", Notifications: config.DesiredBackupNotifications{Worker: "manager", Route: "alerts"}}
		r.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {Routes: []string{"alerts"}, MaxMessages: 500, MaxSilenceSeconds: 180, MaxEvidenceSeconds: 172800, PolicyHealth: true}}
		service := *review.Native
		secrets := map[string][]byte{}
		for field, path := range map[string]string{"signal_account": service.Spec.AccountFile, "signal_recipient": service.Spec.RecipientFile} {
			value, err := ReadCredentialInput(path)
			testutil.Must(t, err)
			secrets[field] = value
		}
		if telegram {
			p := review.Input.Providers["telegram"]
			for field, path := range map[string]string{"token": p.TokenFile, "chat_id": p.ChatFile} {
				value, err := ReadCredentialInput(path)
				testutil.Must(t, err)
				secrets[field] = value
			}
		}
		before := protocol.Hash(r.Desired)
		input, err := CompileFreshSignalNotification(r.Desired, paths, r.Facts.Home, secrets, service)
		testutil.Must(t, err)
		if !input.Bootstrap || len(input.Worker.Producers) != 0 || input.Worker.Interaction != nil || protocol.Hash(r.Desired) != before {
			t.Fatal("notification-first setup fabricated producer authority or changed YAML")
		}
		reservation := backup.ProducerReservation{Version: 1, Producer: protocol.Producer{ID: testutil.ID(t, "w"), Epoch: testutil.ID(t, "n")}, Generation: testutil.ID(t, "g")}
		compiled, err := compileReservedNotification(r.Desired, r.Facts, paths, input, reservation)
		testutil.Must(t, err)
		if compiled.Input.Bootstrap || len(compiled.Input.Worker.Producers) != 1 {
			t.Fatal("reserved producer did not enroll")
		}
		p := compiled.Input.Worker.Producers[0]
		if p.Producer != reservation.Producer || p.MaxMessages != 500 || p.MaxSilenceSeconds != 180 || p.MaxEvidenceSeconds != 172800 {
			t.Fatal("explicit policy was lost during enrollment")
		}
		for _, invalid := range []string{"absent-backup", "disabled-backup", "unknown-producer", "wrong-route", "commands"} {
			candidate := r.Desired
			n, b := *candidate.Notification, *candidate.Backup
			candidate.Notification, candidate.Backup = &n, &b
			switch invalid {
			case "absent-backup":
				candidate.Backup = nil
			case "disabled-backup":
				disabled := false
				b.Enabled = &disabled
			case "unknown-producer":
				n.Producers = map[string]config.ProducerDeclaration{"other-worker": n.Producers["backup-worker"]}
			case "wrong-route":
				b.Notifications.Route = "ungranted"
			case "commands":
				n.Commands.Enabled = true
			}
			if _, err = CompileFreshSignalNotification(candidate, paths, r.Facts.Home, secrets, service); err == nil {
				t.Fatal("unsupported reservation accepted", invalid)
			}
		}
	}
}
