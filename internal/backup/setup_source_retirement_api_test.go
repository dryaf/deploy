package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestSourceRetirementTransportValidatesWholeBatchBeforeEffects(t *testing.T) {
	for _, damage := range []string{"", "endpoint", "missing-admin", "reason", "active", "helper", "source", "worker", "cancelled", "lost-reply", "wrong-ack"} {
		t.Run(damage, func(t *testing.T) {
			worker := testutil.ID(t, "w")
			sources := map[string]SetupSourceRetirement{}
			admins := map[string]config.ServerConfig{}
			d := config.DesiredWorkers{Backup: &config.BackupDeclaration{RetireSources: map[string]string{}}}
			r := WorkerSetupRequest{Declaration: &SetupDeclaration{Desired: &d, Sources: map[string]string{}}}
			for _, alias := range []string{"first", "last"} {
				sources[alias] = SetupSourceRetirement{Target: SetupSourceTarget{alias, alias + ".invalid", "app", 22}, SourceID: testutil.ID(t, "s"), Fingerprint: "retained-key", HelperSHA256: strings.Repeat("a", 64), Reason: "decommissioned"}
				admins[alias] = config.ServerConfig{Host: alias + ".invalid", User: "app"}
				d.Backup.RetireSources[alias] = "decommissioned"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := sources["last"]
			switch damage {
			case "endpoint":
				s.Target.Host = "redirect.invalid"
			case "missing-admin":
				delete(admins, "last")
			case "reason":
				s.Reason = "changed"
			case "active":
				r.Sources = []SetupSource{{Alias: "last"}}
			case "helper":
				s.HelperSHA256 = ""
			case "source":
				s.SourceID = ""
			case "worker":
				worker = ""
			case "cancelled":
				cancel()
			}
			sources["last"] = s
			calls := 0
			got, err := retireSetupSources(ctx, r, admins, worker, sources, func(_ context.Context, target config.ServerConfig, req source.SourceEnrollmentRequest) (source.SourceEnrollmentResult, error) {
				calls++
				if req.Operation != "revoke-retained" || req.WorkerID != worker || req.Previous != "retained-key" || req.HelperSHA256 == "" || req.Reason != "decommissioned" {
					t.Fatal("unpinned revocation", req)
				}
				if damage == "lost-reply" {
					return source.SourceEnrollmentResult{}, errors.New("reply lost after disable")
				}
				ack := source.SourceEnrollmentResult{Schema: 1, SourceID: req.SourceID, WorkerID: req.WorkerID}
				if damage == "wrong-ack" {
					ack.Fingerprint = req.Previous
				}
				return ack, nil
			})
			if damage == "" {
				if err != nil || len(got) != 2 || calls != 2 {
					t.Fatal(got, err, calls)
				}
			} else if err == nil || damage != "lost-reply" && damage != "wrong-ack" && calls != 0 {
				t.Fatal("invalid batch had effects", err, calls)
			}
		})
	}
}

func TestSourceRetirementSetupFramesResumeWithoutEnrollment(t *testing.T) {
	for _, damage := range []string{"", "missing-callback", "wrong-ack", "changed-worker", "unexpected-stage", "missing-final-reply"} {
		t.Run(damage, func(t *testing.T) {
			w, a, s := testutil.ID(t, "w"), testutil.ID(t, "a"), testutil.ID(t, "s")
			response := WorkerSetupResponse{Schema: 1, Stage: "retiring-sources", WorkerID: w, ArchiveID: a, RetiringSources: map[string]SetupSourceRetirement{"old": {SourceID: s}}}
			if damage == "unexpected-stage" {
				response.Stage = "complete"
			}
			complete := WorkerSetupResponse{Schema: 1, Stage: "complete", WorkerID: w, ArchiveID: a}
			if damage == "changed-worker" {
				complete.WorkerID = testutil.ID(t, "w")
			}
			var replies, sent bytes.Buffer
			frame := func(v any) {
				raw, err := json.Marshal(v)
				testutil.Must(t, err)
				testutil.Must(t, protocol.WriteBackupFrame(&replies, raw))
			}
			frame(response)
			if damage != "missing-final-reply" {
				frame(complete)
			}
			calls := 0
			var retire retireSetupSourceBatch = func(worker string, sources map[string]SetupSourceRetirement) (map[string]source.SourceEnrollmentResult, error) {
				calls++
				if worker != w || sources["old"].SourceID != s {
					t.Fatal("changed retirement frame")
				}
				ack := source.SourceEnrollmentResult{Schema: 1, WorkerID: w, SourceID: s}
				if damage == "wrong-ack" {
					ack.SourceID = testutil.ID(t, "s")
				}
				return map[string]source.SourceEnrollmentResult{"old": ack}, nil
			}
			if damage == "missing-callback" {
				retire = nil
			}
			got, err := exchangeWorkerSetupWithRetirement(WorkerSetupRequest{Schema: 1}, &replies, &sent, nil, nil, retire)
			if damage == "" {
				if err != nil || got.Stage != "complete" || calls != 1 {
					t.Fatal(got, err, calls)
				}
			} else if err == nil {
				t.Fatal("invalid retirement exchange completed")
			}
		})
	}
}
