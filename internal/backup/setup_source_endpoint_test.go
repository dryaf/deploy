package backup

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/testutil"
	"golang.org/x/crypto/ssh"
)

func TestSourceEndpointMovePinsUnchangedPolicyAndArtifacts(t *testing.T) {
	for _, change := range []string{"endpoint", "policy", "worker-helper", "source-helper", "image", "source-set", "enrollment-source", "enrollment-worker"} {
		t.Run(change, func(t *testing.T) {
			home, before, _ := sourceExpansionFixture(t)
			raw, err := json.Marshal(before)
			testutil.Must(t, err)
			var request WorkerSetupRequest
			testutil.Must(t, json.Unmarshal(raw, &request))
			request.Sources[0].Host, request.Sources[0].User, request.Sources[0].Port = "moved.invalid", "moveduser", 2222
			switch change {
			case "policy":
				request.Policy.MaxSnapshotBytes++
			case "worker-helper":
				request.Declaration.HelperSHA256 = strings.Repeat("b", 64)
			case "source-helper":
				request.Declaration.Sources[request.Sources[0].Alias] = strings.Repeat("b", 64)
			case "image":
				request.Image = "fixture.invalid/new@sha256:" + strings.Repeat("b", 64)
			case "source-set":
				request.Sources = append(request.Sources, SetupSource{Alias: "extra", Host: "extra.invalid", User: "app", Port: 22})
				request.Policy.Sources = append(request.Policy.Sources, "extra")
			}
			root, state, _, _ := expansionRoots(t, home)
			var j WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &j))
			if strings.HasPrefix(change, "enrollment-") {
				alias := request.Sources[0].Alias
				e := j.SourceEnrollments[alias]
				if change == "enrollment-source" {
					e.SourceID = testutil.ID(t, "s")
				} else {
					e.WorkerID = testutil.ID(t, "w")
				}
				j.SourceEnrollments[alias] = e
				testutil.Must(t, state.WriteJSON("setup.json", j, true))
			}
			old := policyReviewTree(t, home)
			err = beginWorkerSetupUpdate(root, state, &j, request)
			if (err == nil) != (change == "endpoint") {
				t.Fatal(change, err)
			}
			if err != nil && !reflect.DeepEqual(old, policyReviewTree(t, home)) {
				t.Fatal("rejected endpoint move changed installation")
			}
			if err == nil {
				if j.RequestHash == j.PreviousRequestHash || !j.Updating || !reflect.DeepEqual(readOnlySetupSources(j), j.SourceEnrollments) {
					t.Fatal("endpoint move lost inspection authority")
				}
			}
		})
	}
}

func TestSourceEndpointSessionNeverEnrolsRetainedSource(t *testing.T) {
	worker, archive, sourceID := testutil.ID(t, "w"), testutil.ID(t, "a"), testutil.ID(t, "s")
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	testutil.Must(t, err)
	key, err := ssh.NewPublicKey(pub)
	testutil.Must(t, err)
	retained := source.SourceEnrollmentResult{Schema: 1, SourceID: sourceID, WorkerID: worker, Fingerprint: ssh.FingerprintSHA256(key)}
	for _, change := range []string{"same", "wrong-source", "wrong-key", "extra-alias", "missing-inspector", "lost-reply"} {
		t.Run(change, func(t *testing.T) {
			prepared := WorkerSetupResponse{Schema: 1, Stage: "prepared", WorkerID: worker, ArchiveID: archive, PublicKeys: map[string]string{"source": string(ssh.MarshalAuthorizedKey(key))}, ReadOnlySources: map[string]source.SourceEnrollmentResult{"source": retained}}
			if change == "extra-alias" {
				prepared.ReadOnlySources["other"] = retained
			}
			if change == "wrong-key" {
				e := retained
				e.Fingerprint = "other"
				prepared.ReadOnlySources["source"] = e
			}
			var replies, sent bytes.Buffer
			for _, v := range []WorkerSetupResponse{prepared, {Schema: 1, Stage: "source-recorded"}, {Schema: 1, Stage: "complete", WorkerID: worker, ArchiveID: archive}} {
				raw, err := json.Marshal(v)
				testutil.Must(t, err)
				testutil.Must(t, protocol.WriteBackupFrame(&replies, raw))
			}
			calls := 0
			inspect := inspectSetupSource(func(s SetupSource, id, public string, original source.SourceEnrollmentResult) (source.SourceEnrollmentResult, error) {
				calls++
				if s.Host != "moved.invalid" || id != worker || public != prepared.PublicKeys["source"] || original != retained {
					t.Fatal("inspection changed its retained pins")
				}
				if change == "wrong-source" {
					original.SourceID = testutil.ID(t, "s")
				}
				if change == "lost-reply" {
					return original, errors.New("lost read-only inspection reply")
				}
				return original, nil
			})
			if change == "missing-inspector" {
				inspect = nil
			}
			_, err := exchangeWorkerSetup(WorkerSetupRequest{Schema: 1, Sources: []SetupSource{{Alias: "source", Host: "moved.invalid"}}}, &replies, &sent, func(SetupSource, string, string) (source.SourceEnrollmentResult, error) {
				t.Fatal("retained source was enrolled")
				return retained, nil
			}, inspect)
			if (err == nil) != (change == "same") {
				t.Fatal(change, err)
			}
			if calls > 1 {
				t.Fatal("inspection retried automatically")
			}
		})
	}
}
