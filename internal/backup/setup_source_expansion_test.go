//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup/helper"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func sourceExpansionFixture(t *testing.T) (string, WorkerSetupRequest, WorkerSetupRequest) {
	return sourceExpansionFixtureWithHelper(t, false)
}

func sourceExpansionFixtureWithHelper(t *testing.T, actualHelper bool, aliases ...string) (string, WorkerSetupRequest, WorkerSetupRequest) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("rootless source expansion")
	}
	home, before := setupAdoptionFixture(t, aliases...)
	if actualHelper {
		testutil.Must(t, helper.InstallSelf(home, "backup-worker"))
		f, err := os.Open("/proc/self/exe")
		testutil.Must(t, err)
		before.Declaration.HelperSHA256, err = safefs.DigestFile(f)
		f.Close()
		testutil.Must(t, err)
	}
	_, err := runSetupAdoptionFixture(home, before, nil)
	testutil.Must(t, err)
	before.Operation = "apply"
	raw, err := json.Marshal(before)
	testutil.Must(t, err)
	var after WorkerSetupRequest
	testutil.Must(t, json.Unmarshal(raw, &after))
	after.Policy.Sources = append(after.Policy.Sources, "extra")
	after.Sources = append(after.Sources, SetupSource{Alias: "extra", Host: "extra.invalid", User: "app", Port: 22, KnownHosts: before.Sources[0].KnownHosts})
	after.Declaration.Sources["extra"] = strings.Repeat("d", 64)
	after.Declaration.Desired.Backup.Sources = append([]string(nil), after.Policy.Sources...)
	after.Declaration.DesiredHash, err = after.Declaration.Desired.Fingerprint()
	testutil.Must(t, err)
	return home, before, after
}

func TestBackupCombinedSourceReplacementInstallsAndRecoversPublication(t *testing.T) {
	testBackupPolicyPublication(t, "replacement")
}

func TestBackupSourceExpansionInstallsAndRecoversPublication(t *testing.T) {
	testBackupPolicyPublication(t, "sources")
}

func TestBackupSourceEndpointInstallsAndRecoversPublication(t *testing.T) {
	testBackupPolicyPublication(t, "endpoint")
}

func TestBackupMonitoringInstallsAndRecoversPublication(t *testing.T) {
	testBackupPolicyPublication(t, "monitoring")
}

func TestBackupTelegramRotationInstallsAndRecoversPublication(t *testing.T) {
	testBackupPolicyPublication(t, "telegram")
}

func TestBackupTelegramReferencesInstallAndRecoverPublication(t *testing.T) {
	testBackupPolicyPublication(t, "telegram-references")
	testBackupPolicyPublication(t, "telegram-chat-reference")
}

func testBackupPolicyPublication(t *testing.T, change string) {
	t.Helper()
	monitoring, rotation := change == "monitoring", strings.HasPrefix(change, "telegram")
	endpoint := change == "endpoint"
	imageChange := change == "shared-history-image"
	helperChange := change == "shared-history-helper"
	combined := change == "replacement" || change == "shared-history-replacement"
	retiring := change == "retirement" || change == "shared-history-retirement" || combined
	sources := combined || !monitoring && !rotation && !endpoint && !retiring && !imageChange && !helperChange
	for _, shared := range []bool{false, true} {
		if shared && rotation || !shared && strings.HasPrefix(change, "shared-history-") {
			continue
		}
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprint("shared-", shared, "-rollback-", fail), func(t *testing.T) {
				var home string
				var before, r WorkerSetupRequest
				verifyShared := func() {}
				if helperChange {
					home, before, r, verifyShared = sharedHelperHistoryFixture(t)
				} else if strings.HasPrefix(change, "shared-history-") {
					home, before, r, verifyShared = sharedSourceHistoryFixture(t, retiring, combined, imageChange)
				} else if shared && retiring {
					home, before, r, verifyShared = sharedSourceExpansionFixture(t, "source", "retire")
					if combined {
						makeCombinedSourceRequest(t, before, &r)
					} else {
						makeSourceRetirementRequest(t, before, &r)
					}
				} else if shared {
					home, before, r, verifyShared = sharedSourceExpansionFixture(t)
				} else if retiring {
					if combined {
						home, before, r = sourceExpansionFixtureWithHelper(t, true, "source", "retire")
						makeCombinedSourceRequest(t, before, &r)
					} else {
						home, before, r = sourceRetirementFixture(t, true)
					}
				} else {
					home, before, r = sourceExpansionFixtureWithHelper(t, true)
					if monitoring || rotation {
						before = monitoringPublicationFixture(t, home, before)
						r = expandedSourceRequest(t, before)
					}
				}
				if !sources && !retiring {
					// Keep sources and all delivery ownership unchanged. The shared
					// callback retains this declaration pointer for cutover verification.
					r.Policy.Sources, r.Sources = before.Policy.Sources, append([]SetupSource(nil), before.Sources...)
					delete(r.Declaration.Sources, "extra")
					r.Declaration.Desired.Backup.Sources = before.Policy.Sources
					if endpoint {
						r.Sources[0].Host, r.Sources[0].User, r.Sources[0].Port = "moved.invalid", "moveduser", 2222
						r.Sources[0].KnownHosts = strings.ReplaceAll(r.Sources[0].KnownHosts, before.Sources[0].Host, "moved.invalid")
					} else if rotation {
						newToken := filepath.Join(home, "replacement-token")
						testutil.Must(t, os.WriteFile(newToken, []byte("123:replacement-secret"), 0600))
						profile := *r.Policy.Notifications.Telegram
						if change != "telegram-chat-reference" {
							profile.TokenFile = newToken
						}
						if change != "telegram" {
							chat, e := os.ReadFile(profile.ChatIDFile)
							testutil.Must(t, e)
							if change == "telegram-destination" {
								chat = []byte("-100987654321")
							}
							profile.ChatIDFile = filepath.Join(home, "replacement-chat")
							testutil.Must(t, os.WriteFile(profile.ChatIDFile, chat, 0600))
						}
						r.Policy.Notifications.Telegram = &profile
						r.Declaration.Desired.Backup.Notifications.Telegram = &profile
					} else if helperChange {
						// The fixture pins a distinct old helper and the actual candidate.
					} else if imageChange {
						r.Image = "registry.invalid/backup@sha256:" + notify.Hash("replacement image")
						r.Declaration.Desired.Backup.Artifacts.Image = r.Image
					} else {
						backupURL := filepath.Join(home, "backup-monitor")
						deliveryURL := filepath.Join(home, "delivery-monitor")
						testutil.Must(t, os.WriteFile(backupURL, []byte("https://monitor.invalid/backup-secret"), 0600))
						testutil.Must(t, os.WriteFile(deliveryURL, []byte("https://monitor.invalid/delivery-secret"), 0600))
						r.Policy.Monitoring = config.WorkerMonitoring{BackupPingURLFile: backupURL, DailyDeliveryPingURLFile: deliveryURL}
						r.Declaration.Desired.Backup.Monitoring = r.Policy.Monitoring
					}
					var err error
					r.Declaration.DesiredHash, err = r.Declaration.Desired.Fingerprint()
					testutil.Must(t, err)
				}
				root, stateRoot, keys, release := expansionRoots(t, home)
				var j WorkerSetupJournal
				testutil.Must(t, stateRoot.ReadJSON("setup.json", &j))
				j.Files = map[string]setupFileChange{}
				testutil.Must(t, stateRoot.WriteJSON("setup.json", j, true))
				oldWorker, oldArchive, oldRequest := j.WorkerID, j.ArchiveID, j.RequestHash
				oldRuntime, err := os.ReadFile(filepath.Join(home, workerRuntimeConfig))
				testutil.Must(t, err)
				db, err := openInstalledBackupState(home, false)
				testutil.Must(t, err)
				testutil.Must(t, db.put("installation", "active"))
				testutil.Must(t, db.put("min_free_bytes", r.Policy.Storage.MinFreeBytes))
				var deliveryBefore legacyDeliveryJournal
				if rotation {
					installed, e := loadInstalledWorker(home)
					testutil.Must(t, e)
					if change != "telegram-destination" {
						deliveryBefore = rotationDeliveryHistory(t, db, installed)
					}
				}
				db.Close()
				bin := t.TempDir()
				proof, err := json.Marshal(workerSelfTestResult{Schema: 1, Architecture: runtime.GOARCH, Isolated: true, HelperSHA256: r.Declaration.HelperSHA256})
				testutil.Must(t, err)
				podman := "#!/bin/sh\ncase \"$1\" in\n pull) exit 0;;\n run) printf '%s\\n' '" + string(proof) + "';;\n *) exit 1;;\nesac\n"
				testutil.Must(t, os.WriteFile(filepath.Join(bin, "podman"), []byte(podman), 0700))
				generator := filepath.Join(bin, "quadlet-fixture")
				testutil.Must(t, os.WriteFile(generator, []byte("#!/bin/sh\nfor f in \"$QUADLET_UNIT_DIRS\"/*.container; do\n n=${f##*/}; n=${n%.container}; printf '%s\\n' \"---$n.service---\"\ndone\n"), 0700))
				manager := `#!/bin/sh
printf '%s\n' "$*" >> "$BACKUP_EXPANSION_CALLS"
case "$*" in
 *is-enabled*deploy-backup.timer) echo enabled;;
 *is-enabled*) exit 1;;
 *show*) echo loaded;;
 *daemon-reload*)
   if test -f "$BACKUP_EXPANSION_RELOADED" && test -f "$BACKUP_EXPANSION_FAIL"; then
     rm "$BACKUP_EXPANSION_FAIL"; exit 1
   fi
   touch "$BACKUP_EXPANSION_RELOADED";;
 *) exit 0;;
esac
`
				testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte(manager), 0700))
				t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
				t.Setenv("BACKUP_EXPANSION_CALLS", filepath.Join(bin, "calls"))
				t.Setenv("BACKUP_EXPANSION_RELOADED", filepath.Join(bin, "reloaded"))
				t.Setenv("BACKUP_EXPANSION_FAIL", filepath.Join(bin, "fail"))
				if fail {
					testutil.Must(t, os.WriteFile(filepath.Join(bin, "fail"), nil, 0600))
				}
				qualified := map[string]int{}
				tools := workerSetupHostTools{generator: generator, qualifySource: func(s source.WorkerSource, _ config.WorkerTimeouts) error {
					staged, err := os.ReadFile(s.KnownHosts)
					if err != nil || len(staged) == 0 || s.SourceID != j.SourceEnrollments[s.Alias].SourceID || s.KeyFile != filepath.Join(home, workerTrustDirectory, s.Alias+".key") {
						return fmt.Errorf("qualification did not use the reserved source and staged trust")
					}
					qualified[s.Alias]++
					return nil
				}}
				var extra source.SourceEnrollmentResult
				historyOperation := ""
				apply := func() error {
					if err := beginWorkerSetupUpdate(root, stateRoot, &j, r); err != nil {
						return err
					}
					if strings.HasPrefix(change, "shared-history-") {
						if j.SharedPolicyUpdate == nil {
							return fmt.Errorf("missing shared history intent")
						}
						if !imageChange && historyOperation != "" && historyOperation != j.SharedPolicyUpdate.Operation {
							return fmt.Errorf("rollback replaced shared history intent")
						}
						historyOperation = j.SharedPolicyUpdate.Operation
					}
					for _, s := range r.Sources {
						if _, err := setupSourceKey(home, keys, stateRoot, &j, s.Alias); err != nil {
							return err
						}
						if s.Alias == "extra" {
							if extra.SourceID == "" {
								extra = source.SourceEnrollmentResult{Schema: 1, SourceID: testutil.ID(t, "s"), WorkerID: j.WorkerID, Fingerprint: j.SourceKeys[s.Alias]}
							}
							if err := recordSetupSourceEnrollment(stateRoot, &j, s.Alias, extra); err != nil {
								return err
							}
						}
						if err := stageWorkerTrust(home, root, stateRoot, &j, s.Alias, []byte(s.KnownHosts)); err != nil {
							return err
						}
					}
					a, err := openBackupArchive(r.Policy.Storage.Path)
					if err != nil {
						return err
					}
					defer a.Close()
					return finishWorkerSetupUsing(home, root, stateRoot, &j, r, a, tools)
				}
				err = apply()
				if fail {
					if err == nil || (j.SourceExpansion != nil) != sources || j.Updating || j.RecoveryNeeded || j.RequestHash != oldRequest {
						t.Fatal("failed publication did not restore pending expansion", err)
					}
					raw, e := os.ReadFile(filepath.Join(home, workerRuntimeConfig))
					testutil.Must(t, e)
					if string(raw) != string(oldRuntime) || j.SourceEnrollments["extra"] != extra {
						t.Fatal("rollback lost runtime or remote authorization")
					}
					observed, e := inspectSetupLocked(context.Background(), root, home)
					wantStage := "source_enrollment_pending"
					if combined {
						wantStage = "source_replacement_pending"
					}
					if sources && (e != nil || observed.Stage != wantStage) {
						t.Fatal("rollback reported finished source expansion", observed, e)
					}
					if !sources && !shared && !retiring && (e != nil || observed.Stage != "complete" || observed.DesiredHash != before.Declaration.DesiredHash) {
						t.Fatal("monitor rollback did not restore original declaration", observed, e)
					}
					if helperChange {
						if e != nil || observed.Stage != "helper_upgrade_pending" {
							t.Fatal("helper rollback lost resumable state", observed, e)
						}
						if requireSharedHelperRequest(j, before) == nil {
							t.Fatal("rolled-back helper accepted old YAML")
						}
						tree := policyReviewTree(t, home)
						if _, e := inspectSetupUpdateReservation(root, stateRoot, &j, before); e == nil || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
							t.Fatal("old YAML changed pending helper review", e)
						}
						testutil.Must(t, requireSharedHelperRequest(j, r))
					}
					err = apply()
				}
				testutil.Must(t, err)
				if retiring {
					verifyRetirementPublication(t, home, root, stateRoot, &j, r)
				}
				if j.SourceExpansion != nil || j.TelegramRotation != nil || j.Updating || j.RecoveryNeeded || j.Stage != "complete" || j.WorkerID != oldWorker || j.ArchiveID != oldArchive || j.SourceEnrollments["extra"] != extra {
					t.Fatal("completion changed enrollment or archive identity")
				}
				for _, s := range r.Sources {
					want := 1
					if fail {
						want++
					}
					if qualified[s.Alias] != want {
						t.Fatal("installation omitted source qualification", s.Alias)
					}
				}
				installed, err := loadInstalledWorker(home)
				testutil.Must(t, err)
				if len(installed.Sources) != len(r.Sources) || sources && (installed.Policy.Sources[len(installed.Policy.Sources)-1] != "extra" || installed.Sources[len(installed.Sources)-1].SourceID != extra.SourceID) {
					t.Fatal("expanded runtime differs from enrolled sources")
				}
				if imageChange && installed.Image != r.Image {
					t.Fatal("replacement image not published")
				}
				if endpoint {
					got, want := installed.Sources[0], r.Sources[0]
					if got.Host != want.Host || got.User != want.User || got.Port != want.Port || got.SourceID != j.SourceEnrollments[want.Alias].SourceID {
						t.Fatal("endpoint publication lost retained identity")
					}
				}
				if rotation {
					if change != "telegram-destination" {
						verifyRotationDeliveryHistory(t, home, deliveryBefore)
					}
					if !reflect.DeepEqual(installed.Policy.Notifications.Telegram, r.Policy.Notifications.Telegram) || installed.Qualification["notification_delivery"] {
						t.Fatal("rotation lost configured token reference or invented qualification")
					}
					old, e := os.ReadFile(before.Policy.Notifications.Telegram.TokenFile)
					testutil.Must(t, e)
					if string(old) != "123:synthetic-token" {
						t.Fatal("rotation modified old token")
					}
				}
				if monitoring {
					if installed.Policy.Monitoring != r.Policy.Monitoring || installed.Qualification["external_monitor"] {
						t.Fatal("monitor references not installed or observation invented")
					}
					for _, file := range []string{r.Policy.Monitoring.BackupPingURLFile, r.Policy.Monitoring.DailyDeliveryPingURLFile} {
						if file == "" {
							continue
						}
						raw, err := os.ReadFile(file)
						testutil.Must(t, err)
						if !strings.HasPrefix(string(raw), "https://monitor.invalid/") {
							t.Fatal("update changed external monitoring credentials")
						}
					}
				}
				if shared {
					// The shared fixture has a real gateway store but a simulated
					// managed-service profile; inspect its backup stage directly.
					db, err := openInstalledBackupState(home, false)
					testutil.Must(t, err)
					var stage string
					testutil.Must(t, db.get("installation", &stage))
					db.Close()
					if stage != "staged-awaiting-qualification" || installed.Qualification["notification_delivery"] || installed.Qualification["application_restore"] {
						t.Fatal("shared expansion skipped qualification")
					}
				} else {
					observed, err := inspectSetupLocked(context.Background(), root, home)
					if err != nil || observed.Stage != "complete" || observed.Installation != "staged-awaiting-qualification" || len(observed.PendingQualification) == 0 {
						t.Fatal("expanded installation skipped qualification", observed, err)
					}
				}
				calls, err := os.ReadFile(filepath.Join(bin, "calls"))
				testutil.Must(t, err)
				wantRestores := 0
				if fail {
					wantRestores++
				}
				if strings.Count(string(calls), "enable --now deploy-backup.timer") != wantRestores || !strings.Contains(string(calls), "disable deploy-backup.timer") {
					t.Fatal("update activated timers without qualification", string(calls))
				}
				if shared && (strings.Contains(string(calls), "enable --now deploy-backup-notifier") || strings.Contains(string(calls), "start deploy-backup-notifier")) {
					t.Fatal("shared source expansion activated legacy sender")
				}
				release()
				verifyShared()
			})
		}
	}
}

func expansionRoots(t *testing.T, home string) (*safefs.Directory, *safefs.Directory, *safefs.Directory, func()) {
	t.Helper()
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	t.Cleanup(func() { root.Close() })
	lease, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	testutil.Must(t, err)
	t.Cleanup(func() { lease.Close() })
	state, err := root.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, err)
	t.Cleanup(func() { state.Close() })
	keys, err := root.Sub(workerTrustDirectory)
	testutil.Must(t, err)
	t.Cleanup(func() { keys.Close() })
	return root, state, keys, func() { lease.Close() }
}

func TestBackupSourceExpansionPreflightPreservesOwnership(t *testing.T) {
	for _, damage := range []string{"", "shared", "signal", "source-removal", "endpoint", "existing-trust", "source-helper", "worker-helper", "image", "schedule", "duplicate", "missing-key", "changed-receipt", "unowned-key", "unowned-trust"} {
		t.Run(damage, func(t *testing.T) {
			home, _, r := sourceExpansionFixture(t)
			root, state, _, _ := expansionRoots(t, home)
			var j WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &j))
			switch damage {
			case "shared":
				j.Producer = &ProducerReservation{}
			case "signal":
				r.Policy.Notifications.Mode = "shared"
			case "source-removal":
				r.Policy.Sources, r.Sources = r.Policy.Sources[1:], r.Sources[1:]
			case "endpoint":
				r.Sources[0].Host = "different.invalid"
			case "existing-trust":
				r.Sources[0].KnownHosts += "changed\n"
			case "source-helper":
				r.Declaration.Sources[r.Sources[0].Alias] = strings.Repeat("e", 64)
			case "worker-helper":
				r.Declaration.HelperSHA256 = strings.Repeat("e", 64)
			case "image":
				r.Image = "registry.invalid/new@sha256:" + strings.Repeat("e", 64)
			case "schedule":
				r.Policy.Schedule = "daily"
			case "duplicate":
				r.Sources[len(r.Sources)-1] = r.Sources[0]
			case "missing-key":
				testutil.Must(t, os.Remove(filepath.Join(home, workerTrustDirectory, r.Sources[0].Alias+".key")))
			case "changed-receipt":
				e := j.SourceEnrollments[r.Sources[0].Alias]
				e.SourceID = testutil.ID(t, "s")
				j.SourceEnrollments[r.Sources[0].Alias] = e
			case "unowned-key":
				testutil.Must(t, os.WriteFile(filepath.Join(home, workerTrustDirectory, "extra.key"), []byte("unowned"), 0600))
			case "unowned-trust":
				testutil.Must(t, os.WriteFile(filepath.Join(home, workerTrustDirectory, "extra.known_hosts"), []byte("unowned"), 0600))
			}
			testutil.Must(t, state.WriteJSON("setup.json", j, true))
			before := policyReviewTree(t, home)
			err := beginWorkerSetupUpdate(root, state, &j, r)
			if damage == "" {
				testutil.Must(t, err)
				var retained WorkerSetupJournal
				testutil.Must(t, state.ReadJSON("setup.json", &retained))
				if retained.SourceExpansion == nil || retained.SourceExpansion.validate() != nil || retained.SourceExpansion.Before.SourceKeys["extra"] != "" {
					t.Fatal("source expansion not pinned before keys")
				}
				testutil.Must(t, checkSetupSourceExpansion(root, retained, &r))
			} else if err == nil || !reflect.DeepEqual(before, policyReviewTree(t, home)) {
				t.Fatal("invalid source expansion changed retained files", err)
			}
		})
	}
}

func TestBackupSourceExpansionRollbackRetainsEnrollmentAndPinsRetry(t *testing.T) {
	home, oldRequest, r := sourceExpansionFixture(t)
	root, stateRoot, keys, _ := expansionRoots(t, home)
	var j WorkerSetupJournal
	testutil.Must(t, stateRoot.ReadJSON("setup.json", &j))
	oldID, oldArchive, oldRequestHash := j.WorkerID, j.ArchiveID, j.RequestHash
	testutil.Must(t, beginWorkerSetupUpdate(root, stateRoot, &j, r))
	public, err := setupSourceKey(home, keys, stateRoot, &j, "extra")
	testutil.Must(t, err)
	e := source.SourceEnrollmentResult{Schema: 1, WorkerID: j.WorkerID, SourceID: testutil.ID(t, "s"), Fingerprint: j.SourceKeys["extra"]}
	testutil.Must(t, recordSetupSourceEnrollment(stateRoot, &j, "extra", e))
	if j.SourceExpansion.Before.SourceKeys["extra"] != "" || j.SourceExpansion.Before.SourceEnrollments["extra"].SourceID != "" {
		t.Fatal("new enrollment mutated predecessor")
	}
	// Exercise the real rollback completion path. It restores the old local
	// declaration while retaining the new source's already-observed authorization.
	bin := t.TempDir()
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	db, err := openInstalledBackupState(home, false)
	testutil.Must(t, err)
	testutil.Must(t, finishWorkerRollback(stateRoot, &j, db))
	db.Close()
	if j.Stage != "complete" || j.RequestHash != oldRequestHash || j.SourceExpansion == nil || j.SourceEnrollments["extra"] != e {
		t.Fatal("rollback forgot external enrollment")
	}
	testutil.Must(t, checkSetupSourceExpansion(root, j, &r))
	if err = checkSetupSourceExpansion(root, j, &oldRequest); err == nil {
		t.Fatal("old YAML claimed completed source expansion")
	}
	observed, err := inspectSetupLocked(context.Background(), root, home)
	if err != nil || observed.Stage != "source_enrollment_pending" || observed.DesiredHash != r.Declaration.DesiredHash {
		t.Fatal("rollback hid pending enrollment", observed, err)
	}
	testutil.Must(t, beginWorkerSetupUpdate(root, stateRoot, &j, r))
	again, err := setupSourceKey(home, keys, stateRoot, &j, "extra")
	testutil.Must(t, err)
	if public != again || j.WorkerID != oldID || j.ArchiveID != oldArchive {
		t.Fatal("retry replaced ownership")
	}
	testutil.Must(t, recordSetupSourceEnrollment(stateRoot, &j, "extra", e))
	testutil.Must(t, checkSetupSourceExpansion(root, j, &r))
	testutil.Must(t, verifySetupSourceKeys(root, j))
	changed := e
	changed.SourceID = testutil.ID(t, "s")
	before := policyReviewTree(t, home)
	if err = recordSetupSourceEnrollment(stateRoot, &j, "extra", changed); err == nil || !reflect.DeepEqual(before, policyReviewTree(t, home)) {
		t.Fatal("retry replaced the source identity", err)
	}
	// This exercises only journal completion. Full installation still performs
	// restricted-source/image qualification and publishes its runtime first.
	testutil.Must(t, commitWorkerSetupJournal(stateRoot, &j))
	if j.SourceExpansion != nil || j.SourceEnrollments["extra"] != e || j.RequestHash != workerSetupRequestHash(r) || j.WorkerID != oldID || j.ArchiveID != oldArchive {
		t.Fatal("commit lost the expansion result")
	}
	testutil.Must(t, verifySetupSourceKeys(root, j))
}

func TestBackupSourceExpansionPendingRefusesRetargetAndKeyLoss(t *testing.T) {
	for _, damage := range []string{"yaml", "new-endpoint", "new-helper", "new-trust", "missing-old-key", "missing-new-key", "missing-old-receipt", "changed-new-receipt", "predecessor", "unknown-key"} {
		t.Run(damage, func(t *testing.T) {
			home, _, r := sourceExpansionFixture(t)
			root, state, keys, _ := expansionRoots(t, home)
			var j WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &j))
			testutil.Must(t, beginWorkerSetupUpdate(root, state, &j, r))
			_, err := setupSourceKey(home, keys, state, &j, "extra")
			testutil.Must(t, err)
			e := source.SourceEnrollmentResult{Schema: 1, WorkerID: j.WorkerID, SourceID: testutil.ID(t, "s"), Fingerprint: j.SourceKeys["extra"]}
			testutil.Must(t, recordSetupSourceEnrollment(state, &j, "extra", e))
			switch damage {
			case "yaml":
				r.Policy.Schedule = "daily"
			case "new-endpoint":
				r.Sources[len(r.Sources)-1].Host = "another.invalid"
			case "new-helper":
				r.Declaration.Sources["extra"] = strings.Repeat("e", 64)
			case "new-trust":
				r.Sources[len(r.Sources)-1].KnownHosts += "changed"
			case "missing-old-key", "missing-new-key":
				alias := "extra"
				if damage == "missing-old-key" {
					alias = r.Sources[0].Alias
				}
				testutil.Must(t, os.Remove(filepath.Join(home, workerTrustDirectory, alias+".key")))
			case "missing-old-receipt":
				delete(j.SourceEnrollments, r.Sources[0].Alias)
			case "changed-new-receipt":
				e.Fingerprint = "different"
				j.SourceEnrollments["extra"] = e
			case "predecessor":
				j.PreviousRequestHash = strings.Repeat("e", 64)
			case "unknown-key":
				j.SourceKeys["unreserved"] = e.Fingerprint
			}
			testutil.Must(t, state.WriteJSON("setup.json", j, true))
			before := policyReviewTree(t, home)
			if err = checkSetupSourceExpansion(root, j, &r); err == nil || !reflect.DeepEqual(before, policyReviewTree(t, home)) {
				t.Fatal("pending expansion drift was accepted or repaired", err)
			}
		})
	}
}

func TestBackupSourceExpansionInterruptedChild(t *testing.T) {
	home := os.Getenv("DEPLOY_SOURCE_EXPANSION_FIXTURE")
	if home == "" {
		t.Skip("subprocess helper")
	}
	root, state, keys, _ := expansionRoots(t, home)
	var j WorkerSetupJournal
	var r WorkerSetupRequest
	testutil.Must(t, state.ReadJSON("setup.json", &j))
	testutil.Must(t, root.ReadJSON("expansion-request.json", &r))
	testutil.Must(t, beginWorkerSetupUpdate(root, state, &j, r))
	at := os.Getenv("DEPLOY_SOURCE_EXPANSION_BOUNDARY")
	if at != "intent" {
		_, err := setupSourceKey(home, keys, state, &j, "extra")
		testutil.Must(t, err)
	}
	if at == "enrolled" {
		e := source.SourceEnrollmentResult{Schema: 1, WorkerID: j.WorkerID, SourceID: testutil.ID(t, "s"), Fingerprint: j.SourceKeys["extra"]}
		testutil.Must(t, recordSetupSourceEnrollment(state, &j, "extra", e))
	}
	// Kill the coordinator rather than return through defers; the parent checks
	// the real lease release and on-disk intent/keys/enrollment after process death.
	p, err := os.FindProcess(os.Getpid())
	testutil.Must(t, err)
	testutil.Must(t, p.Kill())
}

func TestBackupSourceExpansionProcessDeathRetainsExactIntent(t *testing.T) {
	testSourceExpansionProcessDeath(t, false)
}
func TestBackupCombinedSourceReplacementProcessDeathRetainsBothIntents(t *testing.T) {
	testSourceExpansionProcessDeath(t, true)
}
func testSourceExpansionProcessDeath(t *testing.T, combined bool) {
	for _, shared := range []bool{false, true} {
		for _, at := range []string{"intent", "key", "enrolled"} {
			t.Run(fmt.Sprint("shared-", shared, "-", at), func(t *testing.T) {
				var home string
				var r WorkerSetupRequest
				if combined {
					var before WorkerSetupRequest
					if shared {
						home, before, r, _ = sharedSourceExpansionFixture(t, "source", "retire")
					} else {
						home, before, r = sourceExpansionFixtureWithHelper(t, false, "source", "retire")
					}
					makeCombinedSourceRequest(t, before, &r)
				} else if shared {
					home, _, r, _ = sharedSourceExpansionFixture(t)
				} else {
					home, _, r = sourceExpansionFixture(t)
				}
				root, err := safefs.Open(home)
				testutil.Must(t, err)
				testutil.Must(t, root.WriteJSON("expansion-request.json", r, false))
				root.Close()
				cmd := exec.Command(os.Args[0], "-test.run=^TestBackupSourceExpansionInterruptedChild$")
				cmd.Env = append(os.Environ(), "DEPLOY_SOURCE_EXPANSION_FIXTURE="+home, "DEPLOY_SOURCE_EXPANSION_BOUNDARY="+at)
				out, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(err.Error(), "killed") {
					t.Fatalf("child did not reach SIGKILL boundary: %v %s", err, out)
				}
				root, state, keys, _ := expansionRoots(t, home)
				var j WorkerSetupJournal
				testutil.Must(t, state.ReadJSON("setup.json", &j))
				testutil.Must(t, checkSetupSourceExpansion(root, j, &r))
				if combined {
					if j.SourceRetirement == nil || j.SourceRetirement.Version != 2 {
						t.Fatal("process death lost old source disposition")
					}
					testutil.Must(t, checkSetupSourceRetirement(root, j, &r))
				}
				if j.SourceExpansion == nil || !j.Updating || j.RequestHash != workerSetupRequestHash(r) {
					t.Fatal("process death lost pinned operation")
				}
				pin, enrolled := j.SourceKeys["extra"], j.SourceEnrollments["extra"]
				if (at != "intent") != (pin != "") || (at == "enrolled") != (enrolled.SourceID != "") {
					t.Fatal("unexpected durable boundary", at)
				}
				_, err = setupSourceKey(home, keys, state, &j, "extra")
				testutil.Must(t, err)
				if pin != "" && j.SourceKeys["extra"] != pin || j.SourceEnrollments["extra"] != enrolled {
					t.Fatal("retry changed existing source authority", fmt.Sprint(at))
				}
			})
		}
	}
}

func TestBackupMonitoringPreflightRefusesInvalidPrivateChecks(t *testing.T) {
	for _, damage := range []string{"missing", "public", "http", "same-check"} {
		t.Run(damage, func(t *testing.T) {
			home, r, _ := sourceExpansionFixture(t)
			root, state, _, _ := expansionRoots(t, home)
			var j WorkerSetupJournal
			testutil.Must(t, state.ReadJSON("setup.json", &j))
			file := filepath.Join(home, "new-monitor")
			url := "https://monitor.invalid/private-check-secret"
			mode := os.FileMode(0600)
			if damage == "public" {
				mode = 0644
			}
			if damage == "http" {
				url = "http://monitor.invalid/private-check-secret"
			}
			if damage != "missing" {
				testutil.Must(t, os.WriteFile(file, []byte(url), mode))
			}
			r.Policy.Monitoring.BackupPingURLFile = file
			if damage == "same-check" {
				delivery := filepath.Join(home, "delivery-monitor")
				testutil.Must(t, os.WriteFile(delivery, []byte(url+"/success"), 0600))
				r.Policy.Monitoring.DailyDeliveryPingURLFile = delivery
			}
			r.Declaration.Desired.Backup.Monitoring = r.Policy.Monitoring
			var err error
			r.Declaration.DesiredHash, err = r.Declaration.Desired.Fingerprint()
			testutil.Must(t, err)
			before := policyReviewTree(t, home)
			err = beginWorkerSetupUpdate(root, state, &j, r)
			if err == nil || strings.Contains(err.Error(), "private-check-secret") || !reflect.DeepEqual(before, policyReviewTree(t, home)) {
				t.Fatal("invalid monitor mutated state or exposed secret", err)
			}
		})
	}
}

// Prepare an existing direct-Telegram installation for monitoring migration.
// This is fixture construction only; no provider or service is contacted.
func monitoringPublicationFixture(t *testing.T, home string, before WorkerSetupRequest) WorkerSetupRequest {
	t.Helper()
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var j WorkerSetupJournal
	var c WorkerInstallation
	testutil.Must(t, root.ReadJSON(backupSetupFile, &j))
	testutil.Must(t, root.ReadJSON(workerRuntimeConfig, &c))
	token, chat := filepath.Join(home, "telegram-token"), filepath.Join(home, "telegram-chat")
	testutil.Must(t, os.WriteFile(token, []byte("123:synthetic-token"), 0600))
	testutil.Must(t, os.WriteFile(chat, []byte("456"), 0600))
	c.Policy.Notifications.Mode, c.Policy.Notifications.Primary = "", "telegram"
	c.Policy.Notifications.Telegram = &config.WorkerTelegram{TokenFile: token, ChatIDFile: chat}
	c.Policy, err = config.NormalizeWorker(c.Policy)
	testutil.Must(t, err)
	c.ConfigHash = ""
	c.ConfigHash = notify.Hash(c)
	testutil.Must(t, root.WriteJSON(workerRuntimeConfig, c, true))
	raw, err := safefs.ReadFile(root, workerRuntimeConfig, 8<<20)
	testutil.Must(t, err)
	j.OwnedFiles[workerRuntimeConfig] = sharedSetupDigest(raw)
	before.Policy = c.Policy
	before.Declaration.Desired.Backup.Notifications.WorkerNotifications = c.Policy.Notifications
	before.Declaration.DesiredHash, err = before.Declaration.Desired.Fingerprint()
	testutil.Must(t, err)
	j.Declaration, j.RequestHash = before.Declaration, workerSetupRequestHash(before)
	testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
	state, err := openInstalledBackupState(home, false)
	testutil.Must(t, err)
	defer state.Close()
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	control.ConfigHash, control.ReportingDisabled = c.ConfigHash, false
	control.Revision++
	testutil.Must(t, state.put("control", control))
	a, err := openBackupArchive(c.Policy.Storage.Path)
	testutil.Must(t, err)
	defer a.Close()
	testutil.Must(t, a.commitControl(state))
	return before
}
