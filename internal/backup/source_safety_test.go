//go:build linux

package backup

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupSourceSpoolResumesOwnerRemoval(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "exports"), 0700); err != nil {
		t.Fatal(err)
	}
	id := testutil.ID(t, "b")
	path := filepath.Join(root, "exports", id)
	if err := source.WriteSourceSpoolOwner(path, testutil.ID(t, "s"), id, testutil.ID(t, "n")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, "owner.json")); err != nil {
		t.Fatal(err)
	}
	if err := source.CleanupSourceSpool(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("spool survived resumed cleanup", err)
	}
}

func TestBackupSourceQuotaSurvivesUnfinishedCaptures(t *testing.T) {
	dir, err := safefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	lock, err := dir.Lock("policy.lock", true, true)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	boot, uptime, err := process.HostClock()
	if err != nil {
		t.Fatal(err)
	}
	identity := source.SourceIdentity{ID: testutil.ID(t, "s")}
	worker := testutil.ID(t, "w")
	ledger := source.SourceRateLedger{Schema: 1, SourceID: identity.ID, BootID: boot, WindowUptime: uptime, Workers: map[string]source.SourceRateUsage{worker: {Requests: source.SourceRequestsPerHour - 1, ExportBytes: source.SourceExportBytesPerHour - 10}}}
	if err = dir.WriteJSON("rate.json", ledger, false); err != nil {
		t.Fatal(err)
	}
	if err = source.AdmitSourceRequest(dir, identity, worker); err != nil {
		t.Fatal(err)
	}
	if err = source.AdmitSourceRequest(dir, identity, worker); err == nil {
		t.Fatal("request cap not enforced")
	}
	for i := 0; i < 6; i++ {
		if _, err = source.ReserveSourceCapture(dir, identity, worker); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = source.ReserveSourceCapture(dir, identity, worker); err == nil {
		t.Fatal("unfinished capture reservations were forgiven")
	}
	if err = source.AdmitSourceTransfer(dir, identity, worker, 11); err == nil {
		t.Fatal("export byte cap not enforced")
	}
}

func TestBackupSourceSpoolTemporaryCleanup(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(strconv.FormatBool(foreign), func(t *testing.T) {
			base := t.TempDir()
			path := filepath.Join(base, "exports", testutil.ID(t, "b"))
			testutil.Must(t, source.WriteSourceSpoolOwner(path, testutil.ID(t, "s"), filepath.Base(path), testutil.ID(t, "n")))
			name := "." + testutil.ID(t, "n") + ".json"
			if foreign {
				name = ".n_unknown.json"
			}
			testutil.Must(t, os.WriteFile(filepath.Join(path, name), []byte("interrupted JSON"), 0600))
			err := source.CleanupSourceSpool(path)
			if foreign {
				if err == nil {
					t.Fatal("foreign temporary-looking file was removed")
				}
				_, err = os.Stat(filepath.Join(path, "owner.json"))
				testutil.Must(t, err)
			} else {
				testutil.Must(t, err)
				if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("spool not removed: %v", err)
				}
			}
		})
	}
}
