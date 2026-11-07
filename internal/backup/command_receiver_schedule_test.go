//go:build linux

package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/testutil"
)

func TestScheduledSharedCommandsUsesRetainedReceiptAndBoundedContext(t *testing.T) {
	r := sharedCommandReceiverFixture(t)
	receipt, err := StageSharedCommandReceiver(context.Background(), r)
	testutil.Must(t, err)
	state, err := openBackupState(r.State, false)
	testutil.Must(t, err)
	defer state.Close()
	want := errors.New("receiver unavailable")
	calls := 0
	err = runScheduledSharedCommands(context.Background(), state, func(ctx context.Context, path, pin string) error {
		calls++
		deadline, ok := ctx.Deadline()
		if path != r.State || pin != receipt || !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 8*time.Second {
			t.Fatal("scheduled receiver lost exact authority or budget")
		}
		return want
	})
	if !errors.Is(err, want) || calls != 1 {
		t.Fatal("lost receiver outcome", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = runScheduledSharedCommands(ctx, state, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored cancellation", err)
	}
}

func TestScheduledSharedCommandsRefusesIncompleteOrLostAuthority(t *testing.T) {
	for _, damage := range []string{"pending", "receipt", "sql"} {
		t.Run(damage, func(t *testing.T) {
			r := sharedCommandReceiverFixture(t)
			if damage == "pending" {
				_, err := stageSharedCommandReceiver(context.Background(), r, func(at string) error {
					if at == "intent" {
						return errors.New("interrupted")
					}
					return nil
				})
				if err == nil {
					t.Fatal("setup did not stop")
				}
			} else {
				_, err := StageSharedCommandReceiver(context.Background(), r)
				testutil.Must(t, err)
			}
			state, err := openBackupState(r.State, false)
			testutil.Must(t, err)
			defer state.Close()
			if damage == "receipt" {
				testutil.Must(t, os.Remove(filepath.Join(r.State, commandReceiverFile)))
			}
			if damage == "sql" {
				_, err = state.db.Exec("DROP TABLE backup_command_receiver")
				testutil.Must(t, err)
			}
			err = runScheduledSharedCommands(context.Background(), state, func(context.Context, string, string) error {
				t.Fatal("started damaged receiver")
				return nil
			})
			if err == nil {
				t.Fatal("accepted missing authority")
			}
		})
	}
}

func TestScheduledSharedReportWithoutCommandsAndWithBrokenReceiver(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-commands", true: "broken-receiver"}[broken], func(t *testing.T) {
			f, api, deps, calls := setupReportFixture(t)
			if broken {
				_, err := f.state.db.Exec("CREATE TABLE backup_command_receiver(singleton INTEGER PRIMARY KEY, request_hash TEXT NOT NULL)")
				testutil.Must(t, err)
			}
			err := runSharedBackupReportScheduledWith(context.Background(), f.config, f.state, deps, func(context.Context, string, string) error {
				t.Fatal("started undeclared receiver")
				return nil
			})
			if (err != nil) != broken {
				t.Fatal("incorrect receiver error", err)
			}
			if len(api.accepted) != 1 || len(*calls) != 2 {
				t.Fatal("command failure suppressed backup reporting", len(api.accepted), *calls)
			}
		})
	}
}
