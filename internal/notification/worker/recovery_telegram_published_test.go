package worker

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func publishedTelegramFixture(t *testing.T) (string, Config, RecoveryPublicationResult, TelegramRecoveryCursor) {
	t.Helper()
	f, r, _ := telegramCursorFixture(t)
	ctx := context.Background()
	cursor, err := r.CommitTelegramRecoveryCursor(ctx, f.Account)
	must(t, err)
	_, err = r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	path, c, image := sealPublicationFixture(t, r)
	result, err := publishImage(ctx, path, c, image, nil)
	must(t, err)
	return path, c, result, cursor
}

func TestPublishedTelegramHandoffFreshnessWithoutRenewal(t *testing.T) {
	path, c, published, cursor := publishedTelegramFixture(t)
	image := published.Commit.Image
	retained := filepath.Join(path, "recovery", image.Operation, "checkpoints", image.ReviewHash, publicationDatabase)
	before, err := os.ReadFile(retained)
	must(t, err)
	liveBefore, err := os.ReadFile(filepath.Join(path, "state.db"))
	must(t, err)
	for _, kind := range []string{"valid", "settling", "cutoff", "expired", "mono-expired", "reboot", "wall-back", "mono-back", "wall-jump", "nan", "infinite", "clock-error", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			elapsed := 6 * time.Second
			boot, up := cursor.Drain.BootID, cursor.Drain.Uptime+elapsed.Seconds()
			var clockErr error
			switch kind {
			case "settling":
				elapsed, up = time.Second, cursor.Drain.Uptime+1
			case "cutoff":
				elapsed, up = telegramHandoffSkew, cursor.Drain.Uptime+telegramHandoffSkew.Seconds()
			case "expired":
				elapsed, up = telegramHandoffLifetime, cursor.Drain.Uptime+telegramHandoffLifetime.Seconds()
			case "mono-expired":
				elapsed, up = telegramHandoffLifetime-time.Second, cursor.Drain.Uptime+telegramHandoffLifetime.Seconds()
			case "reboot":
				boot = "changed-boot"
			case "wall-back":
				elapsed = -time.Second
			case "mono-back":
				up = cursor.Drain.Uptime - 1
			case "wall-jump":
				elapsed = time.Minute
			case "nan":
				up = math.NaN()
			case "infinite":
				up = math.Inf(1)
			case "clock-error":
				clockErr = errors.New("clock unavailable")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			at := cursor.Drain.ObservedAt.Add(elapsed)
			result, err := verifyPublishedTelegramHandoff(ctx, path, c, published.Commit.CommitHash, cursor.Drain.Account, func() time.Time { return at }, func() (string, float64, error) {
				// The final clock check still holds exclusive gateway authority.
				d, e := privateDirectory(path)
				must(t, e)
				defer d.Close()
				if lease, e := d.Lock("worker.lock", true, false); e == nil {
					lease.Close()
					t.Fatal("gateway lease released before final clock check")
				}
				if kind == "cancelled" {
					cancel()
				}
				return boot, up, clockErr
			})
			if kind == "valid" {
				must(t, err)
				if result.Cursor != cursor || result.CommitHash != published.Commit.CommitHash || !result.CheckedAt.Equal(at) || !result.ExpiresAt.Equal(cursor.Drain.ObservedAt.Add(telegramHandoffLifetime)) || result.IntakeEnabled || !result.RequiresActivationRecheck {
					t.Fatal(result)
				}
			} else if err == nil || result != (PublishedTelegramHandoff{}) {
				t.Fatal("invalid handoff emitted success", result, err)
			}
		})
	}
	after, err := os.ReadFile(retained)
	must(t, err)
	liveAfter, err := os.ReadFile(filepath.Join(path, "state.db"))
	must(t, err)
	if string(before) != string(after) || string(liveBefore) != string(liveAfter) {
		t.Fatal("handoff inspection rewrote history or live database")
	}
	if s, err := Open(path, c, false); err == nil {
		s.Close()
		t.Fatal("handoff check activated gateway")
	}
	// A completed managed verification adoption may use the live SQLite store;
	// it must preserve the published provider handoff for this inspection.
	ctx := context.Background()
	adoption, err := AdoptPublishedRecoveryVerification(ctx, path, c, published.Commit.CommitHash)
	must(t, err)
	revision, err := safefs.NewID("n")
	must(t, err)
	s, err := OpenManagedRecoveryVerification(path, c, adoption.After, revision, published.Commit.CommitHash)
	must(t, err)
	_, err = s.Health(ctx)
	must(t, err)
	must(t, s.Close())
	_, err = verifyPublishedTelegramHandoff(ctx, path, c, published.Commit.CommitHash, cursor.Drain.Account, func() time.Time {
		return cursor.Drain.ObservedAt.Add(6 * time.Second)
	}, func() (string, float64, error) { return cursor.Drain.BootID, cursor.Drain.Uptime + 6, nil })
	must(t, err)
}

func TestPublishedTelegramHandoffRejectsChangedEvidence(t *testing.T) {
	for _, kind := range []string{"commit", "account", "incomplete", "offset", "missing-offset", "cursor", "rehashed-cursor", "requests", "anchor", "image", "lock"} {
		t.Run(kind, func(t *testing.T) {
			path, c, published, cursor := publishedTelegramFixture(t)
			commit, account := published.Commit.CommitHash, cursor.Drain.Account
			d, err := privateDirectory(path)
			must(t, err)
			defer d.Close()
			switch kind {
			case "commit":
				commit = protocol.Hash("wrong")
			case "account":
				account = "different-account"
			case "incomplete":
				must(t, os.Remove(filepath.Join(path, publicationCommitComplete)))
			case "image":
				im := published.Commit.Image
				must(t, os.WriteFile(filepath.Join(path, "recovery", im.Operation, "checkpoints", im.ReviewHash, publicationDatabase), []byte("changed"), 0600))
			case "lock":
				lease, err := d.Lock("worker.lock", true, false)
				must(t, err)
				defer lease.Close()
			default:
				db, err := snapshotDB(d, "state.db", false)
				must(t, err)
				if kind == "rehashed-cursor" {
					changed := cursor
					changed.Drain.ObservedAt = changed.Drain.ObservedAt.Add(time.Second)
					changed.Drain.Uptime++
					changed.NotBefore = changed.NotBefore.Add(time.Second)
					must(t, changed.Validate())
					body, err := json.Marshal(changed)
					must(t, err)
					_, err = db.Exec("UPDATE reconciliation_telegram_cursors SET hash=?,body=? WHERE account=?", protocol.Hash(changed), string(body), account)
					must(t, err)
					must(t, db.Close())
					break
				}
				query := map[string]string{
					"offset":         "UPDATE telegram_cursors SET offset=999",
					"missing-offset": "DELETE FROM telegram_cursors",
					"cursor":         "UPDATE reconciliation_telegram_cursors SET body='{}'",
					"requests":       "UPDATE reconciliation_request_seal SET body='{}'",
					"anchor":         "DELETE FROM metadata WHERE key='recovery'",
				}[kind]
				_, err = db.Exec(query)
				must(t, err)
				must(t, db.Close())
			}
			result, err := verifyPublishedTelegramHandoff(context.Background(), path, c, commit, account, time.Now, func() (string, float64, error) {
				t.Fatal("clock checked before corrupt evidence was rejected")
				return "", 0, nil
			})
			if err == nil || result != (PublishedTelegramHandoff{}) {
				t.Fatal("changed evidence emitted success", result, err)
			}
		})
	}
}
