//go:build linux

package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func backupTokenRotationFixture(t *testing.T) (string, WorkerSetupRequest) {
	t.Helper()
	home, before, _ := sourceExpansionFixture(t)
	before = monitoringPublicationFixture(t, home, before)
	raw, err := json.Marshal(before)
	testutil.Must(t, err)
	var next WorkerSetupRequest
	testutil.Must(t, json.Unmarshal(raw, &next))
	next.Policy.Notifications.Telegram.TokenFile = filepath.Join(home, "replacement-token")
	testutil.Must(t, os.WriteFile(next.Policy.Notifications.Telegram.TokenFile, []byte("123:next-private-value"), 0600))
	next.Declaration.Desired.Backup.Notifications.Telegram.TokenFile = next.Policy.Notifications.Telegram.TokenFile
	next.Declaration.DesiredHash, err = next.Declaration.Desired.Fingerprint()
	testutil.Must(t, err)
	return home, next
}

func moveBackupChatReference(t *testing.T, home string, r *WorkerSetupRequest, tokenToo bool) {
	t.Helper()
	if !tokenToo {
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		var j WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &j))
		root.Close()
		r.Policy.Notifications.Telegram.TokenFile = declaredSetupTelegram(j.Declaration).TokenFile
	}
	chat, err := os.ReadFile(r.Policy.Notifications.Telegram.ChatIDFile)
	testutil.Must(t, err)
	r.Policy.Notifications.Telegram.ChatIDFile = filepath.Join(home, "replacement-chat")
	testutil.Must(t, os.WriteFile(r.Policy.Notifications.Telegram.ChatIDFile, chat, 0600))
	profile := *r.Policy.Notifications.Telegram
	r.Declaration.Desired.Backup.Notifications.Telegram = &profile
	r.Declaration.DesiredHash, err = r.Declaration.Desired.Fingerprint()
	testutil.Must(t, err)
}

func TestBackupTelegramReferenceMovesKeepAuthority(t *testing.T) {
	for _, tokenToo := range []bool{false, true} {
		for _, damage := range []string{"valid", "other-chat", "new-chat", "old-chat", "shared-token", "missing-pin", "extra-pin", "downgrade"} {
			t.Run(fmt.Sprintf("token-%t/%s", tokenToo, damage), func(t *testing.T) {
				home, r := backupTokenRotationFixture(t)
				moveBackupChatReference(t, home, &r, tokenToo)
				root, state, _, _ := expansionRoots(t, home)
				var j WorkerSetupJournal
				testutil.Must(t, state.ReadJSON("setup.json", &j))
				if damage == "other-chat" {
					testutil.Must(t, os.WriteFile(r.Policy.Notifications.Telegram.ChatIDFile, []byte("different-private-destination"), 0600))
					before := policyReviewTree(t, home)
					if err := beginWorkerSetupUpdate(root, state, &j, r); err == nil || strings.Contains(err.Error(), "private-destination") || !reflect.DeepEqual(before, policyReviewTree(t, home)) {
						t.Fatal("changed destination was accepted, disclosed or mutated", err)
					}
					return
				}
				testutil.Must(t, beginWorkerSetupUpdate(root, state, &j, r))
				testutil.Must(t, checkSetupTelegramRotation(j, &r, false))
				v := j.TelegramRotation
				want := 3
				if tokenToo {
					want = 4
				}
				if v.Version != 2 || len(v.Inputs) != want {
					t.Fatal("reference move lost its input set")
				}
				switch damage {
				case "new-chat":
					testutil.Must(t, os.WriteFile(v.After.ChatIDFile, []byte("changed"), 0600))
				case "old-chat":
					testutil.Must(t, os.WriteFile(v.Before.ChatIDFile, []byte("changed"), 0600))
				case "shared-token":
					testutil.Must(t, os.WriteFile(v.Before.TokenFile, []byte("123:changed"), 0600))
				case "missing-pin":
					delete(v.Inputs, v.After.ChatIDFile)
				case "extra-pin":
					v.Inputs[filepath.Join(home, "unexpected")] = strings.Repeat("e", 64)
				case "downgrade":
					v.Version = 1
				}
				before := policyReviewTree(t, home)
				if err := checkSetupTelegramRotation(j, &r, false); (err == nil) != (damage == "valid") {
					t.Fatal("unexpected forward validation", err)
				}
				// Rollback may disregard candidate-only files, never a shared
				// token path or either old input that will be used after restart.
				if err := checkSetupTelegramRotation(j, nil, true); (err == nil) != (damage == "valid" || damage == "new-chat") {
					t.Fatal("unexpected rollback validation", err)
				}
				if !reflect.DeepEqual(before, policyReviewTree(t, home)) {
					t.Fatal("input validation repaired changed files")
				}
			})
		}
	}
}

func TestBackupTelegramReferenceMovesRejectCrossRolePaths(t *testing.T) {
	for _, newChatUsesOldToken := range []bool{false, true} {
		home, r := backupTokenRotationFixture(t)
		moveBackupChatReference(t, home, &r, true)
		root, state, _, _ := expansionRoots(t, home)
		var j WorkerSetupJournal
		testutil.Must(t, state.ReadJSON("setup.json", &j))
		old := declaredSetupTelegram(j.Declaration)
		if newChatUsesOldToken {
			r.Policy.Notifications.Telegram.ChatIDFile = old.TokenFile
		} else {
			r.Policy.Notifications.Telegram.TokenFile = old.ChatIDFile
		}
		profile := *r.Policy.Notifications.Telegram
		r.Declaration.Desired.Backup.Notifications.Telegram = &profile
		var err error
		r.Declaration.DesiredHash, err = r.Declaration.Desired.Fingerprint()
		testutil.Must(t, err)
		before := policyReviewTree(t, home)
		if err := beginWorkerSetupUpdate(root, state, &j, r); err == nil || !reflect.DeepEqual(before, policyReviewTree(t, home)) {
			t.Fatal("cross-role reference accepted or mutated", err)
		}
	}
}

func TestBackupTelegramRotationRetainsVersionOneCompatibility(t *testing.T) {
	home, r := backupTokenRotationFixture(t)
	root, state, _, _ := expansionRoots(t, home)
	var j WorkerSetupJournal
	testutil.Must(t, state.ReadJSON("setup.json", &j))
	testutil.Must(t, beginWorkerSetupUpdate(root, state, &j, r))
	j.TelegramRotation.Version = 1
	testutil.Must(t, checkSetupTelegramRotation(j, &r, false))
	testutil.Must(t, checkSetupTelegramRotation(j, nil, true))
}

func TestBackupTelegramRotationPreflightAndPendingPins(t *testing.T) {
	for _, damage := range []string{"valid", "other-bot", "invalid-token", "public", "chat-reference", "schedule", "image", "source-helper"} {
		t.Run(damage, func(t *testing.T) {
			home, r := backupTokenRotationFixture(t)
			root, state, _, _ := expansionRoots(t, home)
			var j WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &j))
			switch damage {
			case "other-bot":
				testutil.Must(t, os.WriteFile(r.Policy.Notifications.Telegram.TokenFile, []byte("999:next-private-value"), 0600))
			case "invalid-token":
				testutil.Must(t, os.WriteFile(r.Policy.Notifications.Telegram.TokenFile, []byte("malformed-private-value"), 0600))
			case "public":
				testutil.Must(t, os.Chmod(r.Policy.Notifications.Telegram.TokenFile, 0644))
			case "chat-reference":
				r.Policy.Notifications.Telegram.ChatIDFile = filepath.Join(home, "new-chat")
			case "schedule":
				r.Policy.Schedule = "daily"
			case "image":
				r.Image += "changed"
			case "source-helper":
				r.Declaration.Sources[r.Sources[0].Alias] = strings.Repeat("e", 64)
			}
			before := policyReviewTree(t, home)
			err := beginWorkerSetupUpdate(root, state, &j, r)
			if damage != "valid" {
				if err == nil || strings.Contains(err.Error(), "private-value") || !reflect.DeepEqual(before, policyReviewTree(t, home)) {
					t.Fatal("invalid rotation changed state or disclosed credentials", err)
				}
				return
			}
			testutil.Must(t, err)
			testutil.Must(t, checkSetupTelegramRotation(j, &r, false))
			raw, err := safefs.ReadFile(state, "setup.json", safefs.MaxRecordBytes)
			testutil.Must(t, err)
			if j.TelegramRotation == nil || strings.Contains(string(raw), "private-value") || strings.Contains(string(raw), "synthetic-token") {
				t.Fatal("rotation did not pin inputs privately")
			}
		})
	}
}

func TestBackupTelegramRotationMissingOrChangedAuthorityRefusesResume(t *testing.T) {
	for _, damage := range []string{"new-token", "old-token", "chat", "missing-intent", "predecessor", "request"} {
		t.Run(damage, func(t *testing.T) {
			home, r := backupTokenRotationFixture(t)
			root, state, _, _ := expansionRoots(t, home)
			var j WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &j))
			testutil.Must(t, beginWorkerSetupUpdate(root, state, &j, r))
			switch damage {
			case "new-token":
				testutil.Must(t, os.WriteFile(j.TelegramRotation.After.TokenFile, []byte("123:changed"), 0600))
			case "old-token":
				testutil.Must(t, os.WriteFile(j.TelegramRotation.Before.TokenFile, []byte("123:changed"), 0600))
			case "chat":
				testutil.Must(t, os.WriteFile(j.TelegramRotation.Before.ChatIDFile, []byte("changed-chat"), 0600))
			case "missing-intent":
				j.TelegramRotation = nil
			case "predecessor":
				j.PreviousRequestHash = strings.Repeat("f", 64)
			case "request":
				r.Policy.Schedule = "daily"
			}
			before := policyReviewTree(t, home)
			if err := checkSetupTelegramRotation(j, &r, false); err == nil || !reflect.DeepEqual(before, policyReviewTree(t, home)) {
				t.Fatal("rotation drift accepted or repaired", err)
			}
			if damage == "new-token" {
				// Changed candidate bytes do not prevent restoring the intact old
				// runtime; no candidate sender was activated by publication.
				testutil.Must(t, checkSetupTelegramRotation(j, nil, true))
			} else if damage != "request" {
				if err := checkSetupTelegramRotation(j, nil, true); err == nil {
					t.Fatal("rollback accepted missing or changed old authority")
				}
			}
		})
	}
}

func TestBackupTelegramRotationInterruptedChild(t *testing.T) {
	home := os.Getenv("DEPLOY_BACKUP_TOKEN_ROTATION_HOME")
	if home == "" {
		t.Skip("subprocess helper")
	}
	root, state, _, _ := expansionRoots(t, home)
	var j WorkerSetupJournal
	var r WorkerSetupRequest
	testutil.Must(t, state.ReadJSON("setup.json", &j))
	testutil.Must(t, root.ReadJSON("rotation-request.json", &r))
	testutil.Must(t, beginWorkerSetupUpdate(root, state, &j, r))
	p, err := os.FindProcess(os.Getpid())
	testutil.Must(t, err)
	testutil.Must(t, p.Kill())
}

func TestBackupTelegramRotationProcessDeathKeepsCredentialPins(t *testing.T) {
	testBackupTelegramRotationProcessDeath(t, false)
}

func TestBackupTelegramReferenceMoveProcessDeathKeepsCredentialPins(t *testing.T) {
	testBackupTelegramRotationProcessDeath(t, true)
}

func testBackupTelegramRotationProcessDeath(t *testing.T, moveChat bool) {
	t.Helper()
	home, r := backupTokenRotationFixture(t)
	if moveChat {
		moveBackupChatReference(t, home, &r, true)
	}
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	testutil.Must(t, root.WriteJSON("rotation-request.json", r, false))
	root.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestBackupTelegramRotationInterruptedChild$")
	cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_TOKEN_ROTATION_HOME="+home)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("child did not reach SIGKILL: %v %s", err, out)
	}
	_, state, _, _ := expansionRoots(t, home)
	var j WorkerSetupJournal
	testutil.Must(t, state.ReadJSON("setup.json", &j))
	testutil.Must(t, checkSetupTelegramRotation(j, &r, false))
	if j.TelegramRotation == nil || !j.Updating {
		t.Fatal("process death lost rotation authority")
	}
}

func rotationDeliveryHistory(t *testing.T, state *backupState, c WorkerInstallation) legacyDeliveryJournal {
	t.Helper()
	ctx := context.Background()
	_, release, err := lockLegacyDelivery(ctx, state)
	testutil.Must(t, err)
	defer release()
	j, err := openLegacyDeliveryJournal(ctx, state)
	testutil.Must(t, err)
	for _, id := range []string{"rotation-accepted", "rotation-unknown"} {
		e := notification.NotificationEvent{ID: id, Kind: "test", Message: "retained envelope"}
		ledgerTx(t, state, func(tx *sql.Tx) error { return enqueueNotification(tx, e, time.Now().UTC()) })
		_, eligible, err := j.reserve(ctx, state, c, "telegram", e)
		testutil.Must(t, err)
		if !eligible {
			t.Fatal("fixture reservation missing")
		}
		if id == "rotation-accepted" {
			_, err = j.settle(ctx, state, notification.DeliveryAttempt{Accepted: true}, nil, false, time.Now().UTC())
			testutil.Must(t, err)
		}
	}
	return legacyAttemptRead(t, state)
}

func verifyRotationDeliveryHistory(t *testing.T, home string, before legacyDeliveryJournal) {
	t.Helper()
	state, err := openInstalledBackupState(home, false)
	testutil.Must(t, err)
	defer state.Close()
	after := legacyAttemptRead(t, state)
	if after.head != before.head || notify.Hash(after.pending) != notify.Hash(before.pending) || after.review.Accepted != 1 || after.review.Unsettled != 1 || after.review.Attempts != 2 {
		t.Fatal("rotation changed prior delivery evidence", after.review)
	}
}

func TestBackupTelegramRotationRollbackRefusesOldCredentialDriftBeforeRestart(t *testing.T) {
	home, r := backupTokenRotationFixture(t)
	root, stateRoot, _, _ := expansionRoots(t, home)
	var j WorkerSetupJournal
	testutil.Must(t, stateRoot.ReadJSON("setup.json", &j))
	testutil.Must(t, beginWorkerSetupUpdate(root, stateRoot, &j, r))
	testutil.Must(t, os.WriteFile(j.TelegramRotation.Before.TokenFile, []byte("123:changed-old-token"), 0600))
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	t.Setenv("BACKUP_ROTATION_MANAGER_CALLS", calls)
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nprintf called >> \"$BACKUP_ROTATION_MANAGER_CALLS\"\n"), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	state, err := openInstalledBackupState(home, false)
	testutil.Must(t, err)
	defer state.Close()
	if err = finishWorkerRollback(stateRoot, &j, state); err == nil {
		t.Fatal("rollback accepted changed predecessor credential")
	}
	if _, err = os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("rollback restarted activity with changed credentials")
	}
}
