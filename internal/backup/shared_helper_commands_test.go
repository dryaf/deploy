//go:build linux

package backup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/testutil"
)

func TestSharedHelperCommandReviewRetainsAuthority(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		for _, damage := range []string{"", "receipt", "lock", "ledger", "binding", "missing-review", "capability", "wal", "schema-lock"} {
			t.Run(strings.Join([]string{map[bool]string{false: "read", true: "mutation"}[mutation], damage}, "/"), func(t *testing.T) {
				r, _ := sharedCommandReceiverModeFixture(t, mutation)
				_, err := StageSharedCommandReceiver(context.Background(), r)
				testutil.Must(t, err)
				state, err := openBackupState(r.State, false)
				testutil.Must(t, err)
				owner, err := currentSharedBackupOwner(state)
				testutil.Must(t, err)
				if damage == "ledger" {
					_, err = state.db.Exec("DROP TABLE interactive_results")
					testutil.Must(t, err)
				}
				if damage == "binding" {
					_, err = state.db.Exec("UPDATE backup_command_receiver SET request_hash='lost'")
					testutil.Must(t, err)
				}
				testutil.Must(t, state.Close())
				home := strings.TrimSuffix(r.State, "/.local/state/deploy/backup-worker/state")
				j := WorkerSetupJournal{WorkerID: r.Producer.ID, ArchiveID: r.Archive, Declaration: &SetupDeclaration{Version: 1, Home: home}, Producer: &ProducerReservation{Version: 1, Producer: r.Producer, Generation: owner.Binding.Generation}, Shared: &sharedWorkerSetup{Version: 1, Owner: *owner}}
				p, err := declarationPaths(home, j.Declaration)
				testutil.Must(t, err)
				c := &sharedHelperCommands{Receiver: commandReceiverRecord{Request: r, Complete: true}, Capability: "backup-shared-commands-v2"}
				switch damage {
				case "receipt":
					testutil.Must(t, os.Remove(filepath.Join(r.State, commandReceiverFile)))
				case "lock":
					testutil.Must(t, os.Remove(filepath.Join(r.State, "commands.lock")))
				case "missing-review":
					c = nil
				case "capability":
					c.Capability = "unsupported"
				case "wal":
					testutil.Must(t, os.WriteFile(filepath.Join(r.State, "worker.db-wal"), []byte("retained WAL"), 0600))
				case "schema-lock":
					testutil.Must(t, os.Remove(filepath.Join(r.State, "schema.lock")))
				}
				tree := policyReviewTree(t, home)
				err = inspectSharedHelperCommands(p, j, c, false)
				if (err == nil) != (damage == "") {
					t.Fatal(damage, err)
				}
				if !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
					t.Fatal("receiver review changed retained state")
				}
			})
		}
	}
}
