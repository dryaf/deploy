package backup

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/backup/protocol"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/crypto/ssh"
)

func TestSetupSessionRetainsIdentityAndCompletedRetry(t *testing.T) {
	worker, _ := safefs.NewID("w")
	archive, _ := safefs.NewID("a")
	source, _ := safefs.NewID("s")
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	prepared := WorkerSetupResponse{Schema: 1, Stage: "prepared", WorkerID: worker, ArchiveID: archive, PublicKeys: map[string]string{"vps": string(ssh.MarshalAuthorizedKey(key))}}
	complete := WorkerSetupResponse{Schema: 1, Stage: "complete", WorkerID: worker, ArchiveID: archive}
	request := WorkerSetupRequest{Schema: 1, Sources: []SetupSource{{Alias: "vps"}}}
	for _, retry := range []bool{false, true} {
		var replies, sent bytes.Buffer
		frame := func(v any) {
			b, e := json.Marshal(v)
			if e == nil {
				e = protocol.WriteBackupFrame(&replies, b)
			}
			if e != nil {
				t.Fatal(e)
			}
		}
		if !retry {
			frame(prepared)
			frame(WorkerSetupResponse{Schema: 1, Stage: "source-recorded"})
		}
		frame(complete)
		calls := 0
		result, e := exchangeWorkerSetup(request, &replies, &sent, func(s SetupSource, id, pub string) (deploysource.SourceEnrollmentResult, error) {
			calls++
			if s.Alias != "vps" || id != worker || pub != prepared.PublicKeys["vps"] {
				t.Fatal("enrollment changed prepared identity")
			}
			return deploysource.SourceEnrollmentResult{Schema: 1, WorkerID: worker, SourceID: source, Fingerprint: ssh.FingerprintSHA256(key)}, nil
		})
		if e != nil || result.WorkerID != worker || result.ArchiveID != archive || result.Stage != "complete" {
			t.Fatal(result, e)
		}
		if retry && calls != 0 || !retry && calls != 1 {
			t.Fatal("completed retry repeated source mutation")
		}
		frames := 0
		for {
			_, e := protocol.ReadBackupFrame(&sent)
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
			frames++
		}
		if retry && frames != 1 || !retry && frames != 3 {
			t.Fatal("unexpected request chain", frames)
		}
	}
}

func TestDeclarativeSetupReservationDoesNotAdoptOrReplaceLostAuthority(t *testing.T) {
	for _, present := range []string{".local/state/deploy/locks/backup-worker-setup.lock", ".local/state/deploy/backup-worker/setup.json", ".config/deploy/backup-worker/runtime.json"} {
		t.Run(filepath.Base(present), func(t *testing.T) {
			home := t.TempDir()
			root, err := safefs.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err = checkDeclarativeSetupReservation(root); err != nil {
				t.Fatal(err)
			}
			if err = root.Mkdir(filepath.Dir(present)); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(home, present), []byte("retained"), 0600); err != nil {
				t.Fatal(err)
			}
			if err = checkDeclarativeSetupReservation(root); err == nil {
				t.Fatal("unowned or lost installation was admitted")
			}
			body, err := os.ReadFile(filepath.Join(home, present))
			if err != nil || string(body) != "retained" {
				t.Fatal("inspection changed retained evidence", err)
			}
		})
	}
}

func TestSetupSessionRejectsBrokenResponseChain(t *testing.T) {
	worker, _ := safefs.NewID("w")
	archive, _ := safefs.NewID("a")
	changed, _ := safefs.NewID("w")
	for _, response := range []WorkerSetupResponse{
		{Schema: 1, Stage: "complete", WorkerID: changed, ArchiveID: archive},
		{Schema: 2, Stage: "complete", WorkerID: worker, ArchiveID: archive},
		{Schema: 1, Stage: "prepared", WorkerID: worker, ArchiveID: archive},
		{Schema: 1, Stage: "complete", WorkerID: worker, ArchiveID: archive, Error: "secret fixture must not leak"},
	} {
		var replies, sent bytes.Buffer
		for _, v := range []WorkerSetupResponse{{Schema: 1, Stage: "prepared", WorkerID: worker, ArchiveID: archive}, response} {
			b, _ := json.Marshal(v)
			if err := protocol.WriteBackupFrame(&replies, b); err != nil {
				t.Fatal(err)
			}
		}
		_, err := exchangeWorkerSetup(WorkerSetupRequest{Schema: 1}, &replies, &sent, nil)
		if err == nil || bytes.Contains([]byte(err.Error()), []byte("secret fixture")) {
			t.Fatal("invalid response accepted or leaked", err)
		}
	}
}
