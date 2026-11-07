package backup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestDeclarativeProducerReservationResumesPublicationAndRejectsLoss(t *testing.T) {
	dir := t.TempDir()
	d, err := safefs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	operation, _ := safefs.NewID("n")
	workerID, _ := safefs.NewID("w")
	archive, _ := safefs.NewID("a")
	j := WorkerSetupJournal{Schema: 1, ID: operation, WorkerID: workerID, ArchiveID: archive, Stage: "started", RequestHash: strings.Repeat("a", 64), Declaration: &SetupDeclaration{Version: 1}}
	if err = d.WriteJSON("setup.json", j, false); err != nil {
		t.Fatal(err)
	}
	r, err := reserveSetupProducer(d, &j)
	if err != nil || r.Validate(workerID) != nil {
		t.Fatal(r, err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "producer-reservation.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate death after intent publication but before the setup record update.
	j.Producer = nil
	if err = d.WriteJSON("setup.json", j, true); err != nil {
		t.Fatal(err)
	}
	if err = checkSetupProducerReservation(d, &j); err != nil || j.Producer == nil || *j.Producer != r {
		t.Fatal(j, err)
	}
	for _, stage := range []string{"started", "prepared", "complete"} {
		j.Stage = stage
		again, err := reserveSetupProducer(d, &j)
		if err != nil || again != r {
			t.Fatal("retry replaced reservation", again, err)
		}
	}
	j.Declaration.NotificationRoot, j.Declaration.NotificationRoute = "/home/fixture/gateway", "alerts"
	j.Shared = &sharedWorkerSetup{GatewayRoot: j.Declaration.NotificationRoot, Owner: backupNotificationOwner{Mode: "shared", Binding: backupNotificationBinding{Producer: r.Producer, Generation: r.Generation, Route: "alerts"}}}
	if again, err := reserveSetupProducer(d, &j); err != nil || again != r {
		t.Fatal("completed shared setup replaced its reservation", again, err)
	}
	j.Shared.Owner.Binding.Generation, _ = safefs.NewID("g")
	if _, err := reserveSetupProducer(d, &j); err == nil {
		t.Fatal("reserved across a changed reporting generation")
	}
	j.Shared = nil
	after, err := os.ReadFile(filepath.Join(dir, "producer-reservation.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("retry rewrote intent", err)
	}
	if err = os.Remove(filepath.Join(dir, "producer-reservation.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = reserveSetupProducer(d, &j); err == nil {
		t.Fatal("lost reservation admitted")
	}
	if err = checkSetupProducerReservation(d, &j); err == nil {
		t.Fatal("setup admitted lost reservation")
	}
	if _, err = os.Stat(filepath.Join(dir, "producer-reservation.json")); !os.IsNotExist(err) {
		t.Fatal("missing authority recreated")
	}
}

func TestSetupSessionReservationNeverEnrollsSources(t *testing.T) {
	w, _ := safefs.NewID("w")
	a, _ := safefs.NewID("a")
	e, _ := safefs.NewID("n")
	g, _ := safefs.NewID("g")
	r := ProducerReservation{Version: 1, Generation: g}
	r.Producer.ID, r.Producer.Epoch = w, e
	for _, stage := range []string{"reserved", "prepared", "complete"} {
		var out, in bytes.Buffer
		body, _ := json.Marshal(WorkerSetupResponse{Schema: 1, Stage: stage, WorkerID: w, ArchiveID: a, Producer: &r})
		if err := protocol.WriteBackupFrame(&out, body); err != nil {
			t.Fatal(err)
		}
		got, err := exchangeWorkerSetup(WorkerSetupRequest{Schema: 1, Operation: "reserve", Sources: []SetupSource{{Alias: "vps"}}}, &out, &in, func(SetupSource, string, string) (source.SourceEnrollmentResult, error) {
			t.Fatal("reservation enrolled source")
			return source.SourceEnrollmentResult{}, nil
		})
		if stage == "reserved" {
			if err != nil || got.Producer == nil || *got.Producer != r {
				t.Fatal(got, err)
			}
		} else if err == nil {
			t.Fatal("invalid reservation stage accepted", stage)
		}
		if _, err := protocol.ReadBackupFrame(&in); err != nil {
			t.Fatal(err)
		}
		if in.Len() != 0 {
			t.Fatal("reservation sent an enrollment or finish request")
		}
	}
}
