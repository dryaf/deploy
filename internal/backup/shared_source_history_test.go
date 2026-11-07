//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestSharedSourceAdditionAfterPolicyHistoryPublishesAndRollsBack(t *testing.T) {
	testBackupPolicyPublication(t, "shared-history-sources")
}

func sharedSourceHistoryFixture(t *testing.T, retirement ...bool) (string, WorkerSetupRequest, WorkerSetupRequest, func()) {
	t.Helper()
	var aliases []string
	retiring := len(retirement) != 0 && retirement[0]
	combined := len(retirement) > 1 && retirement[1]
	imageChange := len(retirement) > 2 && retirement[2]
	if retiring {
		aliases = []string{"source", "retire"}
	}
	home, r, original, deps, _, profile := providerScopeFixtureWithSources(t, aliases)
	// A completed real installation already owns its recovery supervisor.
	// Seed those files before freezing notification history; otherwise a first
	// update adds fixture-only ownership that deliberately survives rollback.
	bin := t.TempDir()
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	initialRoot, e := safefs.Open(home)
	testutil.Must(t, e)
	initialState, e := initialRoot.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, e)
	original.ArchivePath = original.Declaration.Desired.Backup.Storage.Path
	testutil.Must(t, installRecoverySupervisor(home, initialRoot, initialState, &original))
	initialState.Close()
	initialRoot.Close()
	r.Desired.Notification.Providers = original.Declaration.Desired.Notification.Providers
	r.Desired.Notification.Limits.MaxMessages = 9000
	deps.credentials = slices.Clone(profile.Credentials)
	_, err := reconcileNotificationScopeReports(context.Background(), home, r, func(ProducerReservation) (string, error) {
		profile.ManifestHash = notify.Hash("shared expansion fixture limits")
		return profile.ManifestHash, nil
	}, 17, deps)
	testutil.Must(t, err)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var j WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &j))
	c, err := loadInstalledWorker(home)
	testutil.Must(t, err)
	before := WorkerSetupRequest{Schema: 1, Operation: "apply", Declaration: j.Declaration, Policy: c.Policy, Image: c.Image, RelayImage: c.RelayImage}
	for _, s := range c.Sources {
		known, e := os.ReadFile(s.KnownHosts)
		testutil.Must(t, e)
		before.Sources = append(before.Sources, SetupSource{s.Alias, s.Host, s.User, s.Port, string(known)})
	}
	// The actual notification operation already produced this request hash.
	if workerSetupRequestHash(before) != j.RequestHash {
		t.Fatal("history fixture request differs")
	}
	after := expandedSourceRequest(t, before)
	if combined {
		makeCombinedSourceRequest(t, before, &after)
	} else if retiring {
		makeSourceRetirementRequest(t, before, &after)
	}
	cutoverPath := filepath.Join(backupCutoverPaths(home, c).State, backupCutoverFile)
	cutover, err := os.ReadFile(cutoverPath)
	testutil.Must(t, err)
	verify := func() {
		t.Helper()
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		defer root.Close()
		var current WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &current))
		installed, e := loadInstalledWorker(home)
		testutil.Must(t, e)
		prior, projected, e := reportingOrigin(root, current, installed, original.Declaration.DesiredHash, original.Declaration.NotificationDesiredHash, notify.Hash(c), original.Declaration.HelperSHA256)
		testutil.Must(t, e)
		if prior.RequestHash != original.RequestHash || !reflect.DeepEqual(projected, c) {
			t.Fatal("addition lost original reporting runtime")
		}
		retained, e := os.ReadFile(cutoverPath)
		testutil.Must(t, e)
		if string(retained) != string(cutover) {
			t.Fatal("addition replaced original reporting handoff")
		}
		var originalCutover backupCutoverIntent
		testutil.Must(t, json.Unmarshal(cutover, &originalCutover))
		spec := originalCutover.Spec
		spec.Version, spec.Predecessor, spec.CompletedPolicyHash = 7, notify.Hash(originalCutover), current.Declaration.DesiredHash
		spec.ConfigHash = notify.Hash(installed)
		testutil.Must(t, requireFreshBackupReportingSetup(root, home, current, spec))
		if retiring {
			receipt, e := readSharedPolicyHistory(root, current.SharedPolicyHistory)
			testutil.Must(t, e)
			expectedVersion := 4
			if combined {
				expectedVersion = 5
				if receipt.Expansion == nil {
					t.Fatal("replacement lost enrollment intent")
				}
				changed := receipt
				changed.Expansion = nil
				if changed.validate() == nil {
					t.Fatal("replacement accepted lost enrollment intent")
				}
			}
			if receipt.Intent.Version != expectedVersion || receipt.Retirement == nil || receipt.Revocations["retire"].SourceID == "" {
				t.Fatal("retirement lost confirmed disposition")
			}
			receipt.Revocations = nil
			if receipt.validate() == nil {
				t.Fatal("unconfirmed retirement accepted")
			}
			return
		}
		if imageChange {
			receipt, e := readSharedPolicyHistory(root, current.SharedPolicyHistory)
			testutil.Must(t, e)
			if receipt.Intent.Version != 6 || receipt.Expansion != nil || receipt.Retirement != nil {
				t.Fatal("image history lost its scope")
			}
			if installed.Image == c.Image || !reflect.DeepEqual(installed.Sources, c.Sources) {
				t.Fatal("image update lost sources or kept the old image")
			}
			return
		}
		key := filepath.Join(home, workerTrustDirectory, "extra.key")
		testutil.Must(t, os.Rename(key, key+".retained"))
		if e := requireFreshBackupReportingSetup(root, home, current, spec); e == nil {
			t.Fatal("reporting continuation ignored missing added source key")
		}
		testutil.Must(t, os.Rename(key+".retained", key))
		receipt, e := readSharedPolicyHistory(root, current.SharedPolicyHistory)
		testutil.Must(t, e)
		if receipt.Intent.Version != 3 || receipt.Expansion == nil {
			t.Fatal("missing enrollment history")
		}
		altered := receipt
		altered.Expansion = nil
		if altered.validate() == nil {
			t.Fatal("lost source expansion accepted")
		}
		raw, e := json.Marshal(receipt)
		testutil.Must(t, e)
		altered = sharedPolicyReceipt{}
		testutil.Must(t, json.Unmarshal(raw, &altered))
		enrollment := altered.After.SourceEnrollments["extra"]
		enrollment.WorkerID = testutil.ID(t, "w")
		altered.After.SourceEnrollments["extra"] = enrollment
		if altered.validate() == nil {
			t.Fatal("foreign source acknowledgement accepted")
		}
	}
	return home, before, after, verify
}

func TestSharedSourceRetirementAfterPolicyHistoryPublishesAndRollsBack(t *testing.T) {
	testBackupPolicyPublication(t, "shared-history-retirement")
}

func TestSharedSourceReplacementAfterPolicyHistoryPublishesAndRollsBack(t *testing.T) {
	testBackupPolicyPublication(t, "shared-history-replacement")
}

func TestSharedBackupImageAfterPolicyHistoryPublishesAndRollsBack(t *testing.T) {
	testBackupPolicyPublication(t, "shared-history-image")
}
