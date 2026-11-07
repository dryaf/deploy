//go:build linux

package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestSharedCommandReceiverReviewUsesCompletedReportingAndEnrolledPolicy(t *testing.T) {
	for _, change := range []string{"", "pending-reporting", "desired", "helper", "policy", "generation", "mutation", "completion-lost"} {
		t.Run(change, func(t *testing.T) {
			home, c, desired, manifest, reporting, session := freshSharedReportingFixture(t)
			ctx := context.Background()
			if change != "pending-reporting" {
				testutil.Must(t, connectSharedReporting(ctx, home, desired, manifest, reporting))
			}
			destination := interaction.Destination{ID: "private", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"}
			policy := interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "backup/manager", Producer: session.p, Generation: session.g, Commands: BackupCommandDescriptors()}}, Destinations: []interaction.Destination{destination}, Permissions: []interaction.Permission{{Actor: "789", Conversation: destination.ID, Worker: session.p.ID, Permissions: []string{"read"}, Scope: interaction.Scope{Subjects: []string{"a"}}}}}
			if change == "generation" {
				policy.Workers[0].Generation = "different"
			}
			if change == "mutation" {
				policy.Workers[0].Commands = backupCommandManifest(true)
			}
			if change == "desired" {
				desired = notify.Hash("different")
			}
			deps := commandReceiverReviewDependencies{reporting: reporting,
				helper: func(_ context.Context, gotHome string, got WorkerInstallation, hash string, mutations bool) error {
					if gotHome != home || notify.Hash(got) != notify.Hash(c) || !safefs.DigestPattern.MatchString(hash) {
						t.Fatal("lost helper review pins")
					}
					if change == "helper" {
						return errors.New("old helper")
					}
					return nil
				},
				policy: func(_ context.Context, root, hash string, p notify.Producer) (interaction.Config, error) {
					if root != session.m.Root || hash != manifest || p != session.p {
						t.Fatal("lost gateway review pins")
					}
					if change == "policy" {
						return interaction.Config{}, errors.New("changed gateway")
					}
					if change == "completion-lost" {
						state, err := openBackupState(backupCutoverPaths(home, c).State, false)
						testutil.Must(t, err)
						_, err = state.db.Exec("DELETE FROM meta WHERE key='notification_cutover_complete'")
						testutil.Must(t, err)
						testutil.Must(t, state.Close())
					}
					return policy, nil
				}}
			op, err := safefs.NewID("n")
			testutil.Must(t, err)
			r, err := reviewSharedCommandReceiver(ctx, home, desired, manifest, op, deps)
			if change != "" && change != "mutation" {
				if err == nil {
					t.Fatal("accepted changed receiver authority")
				}
			} else {
				testutil.Must(t, err)
				if r.Operation != op || r.Producer != session.p || r.Identity != session.m.Identity || notify.Hash(r.Policy) != notify.Hash(policy) {
					t.Fatal("lost reviewed authority")
				}
				if r.mutations() != (change == "mutation") {
					t.Fatal("receiver did not pin mutation authority")
				}
				again, err := reviewSharedCommandReceiver(ctx, home, desired, manifest, op, deps)
				testutil.Must(t, err)
				if notify.Hash(again) != notify.Hash(r) {
					t.Fatal("review is unstable")
				}
			}
			for _, name := range []string{commandReceiverFile, "commands.lock"} {
				if _, err := os.Stat(filepath.Join(backupCutoverPaths(home, c).State, name)); !os.IsNotExist(err) {
					t.Fatal("review installed receiver", name, err)
				}
			}
		})
	}
}

func TestSharedCommandReceiverHelperCapabilityRequiresExactOwnedExecutable(t *testing.T) {
	for _, change := range []string{"", "old-helper", "hash", "permissions", "mutation-v1", "mutation-v2"} {
		t.Run(change, func(t *testing.T) {
			home, c, _, _, _, _ := freshSharedReportingFixture(t)
			path := backupCutoverPaths(home, c).helperFile()
			testutil.Must(t, os.MkdirAll(filepath.Dir(path), 0700))
			body := "#!/bin/sh\n[ \"$1\" = backup-worker ] && [ \"$2\" = __shared-commands-probe ] && printf 'backup-shared-commands-v1\\n'\n"
			if change == "mutation-v2" {
				body = strings.Replace(body, "commands-v1", "commands-v2", 1)
			}
			if change == "old-helper" {
				body = "#!/bin/sh\nexit 1\n"
			}
			testutil.Must(t, os.WriteFile(path, []byte(body), 0700))
			testutil.Must(t, os.Chmod(path, 0700))
			hash := sharedSetupDigest([]byte(body))
			if change == "hash" {
				hash = notify.Hash("different")
			}
			if change == "permissions" {
				testutil.Must(t, os.Chmod(path, 0777))
			}
			err := inspectCommandReceiverHelper(context.Background(), home, c, hash, strings.HasPrefix(change, "mutation-"))
			if (err != nil) != (change != "" && change != "mutation-v2") {
				t.Fatal("incorrect helper capability review", err)
			}
		})
	}
}

func TestSharedCommandReceiverInspectionNeverCompletesSetup(t *testing.T) {
	for _, at := range []string{"intent", "ledger", "complete"} {
		t.Run(at, func(t *testing.T) {
			r := sharedCommandReceiverFixture(t)
			ctx := context.Background()
			pin, complete, err := InspectSharedCommandReceiver(ctx, r)
			testutil.Must(t, err)
			if pin != "" || complete {
				t.Fatal("uninstalled receiver was complete")
			}
			if _, err = os.Stat(filepath.Join(r.State, "commands.lock")); !os.IsNotExist(err) {
				t.Fatal("inspection initialized command lock", err)
			}
			stop := errors.New("interrupted")
			_, err = stageSharedCommandReceiver(ctx, r, func(boundary string) error {
				if at == boundary {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(r.State, commandReceiverFile))
			testutil.Must(t, err)
			pin, complete, err = InspectSharedCommandReceiver(ctx, r)
			testutil.Must(t, err)
			if complete != (at == "complete") || complete && !safefs.DigestPattern.MatchString(pin) {
				t.Fatal("incorrect pending inspection")
			}
			after, err := os.ReadFile(filepath.Join(r.State, commandReceiverFile))
			testutil.Must(t, err)
			if string(before) != string(after) {
				t.Fatal("inspection completed setup")
			}
			receipt, err := StageSharedCommandReceiver(ctx, r)
			testutil.Must(t, err)
			pin, complete, err = InspectSharedCommandReceiver(ctx, r)
			testutil.Must(t, err)
			if !complete || pin != receipt {
				t.Fatal("lost completed receipt")
			}
			testutil.Must(t, os.Remove(filepath.Join(r.State, commandReceiverFile)))
			if _, _, err = InspectSharedCommandReceiver(ctx, r); err == nil {
				t.Fatal("ignored lost receipt")
			}
		})
	}
}
