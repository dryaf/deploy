package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestCommandRecoveryPullSkipsHistoricalPages(t *testing.T) {
	f, _ := commandInstallFixture(t, false)
	ctx := context.Background()
	_, err := interaction.InstallCommandRecovery(ctx, f.ledger, f.r)
	must(t, err)
	// Supply more than one scan page of historical IDs, then one new command.
	// Use a borrowed candidate solely to test runtime filtering, not activation.
	var fresh interaction.Command
	must(t, f.r.transaction(ctx, func(tx *sql.Tx) error {
		for i := 0; i < 130; i++ {
			c := f.first
			c.ID = fmt.Sprintf("new-%d", i)
			c.RequestKey = protocol.Hash(c.ID)
			c.InputHash = protocol.Hash(c.RequestKey)
			c.IssuedAt = time.Now().UTC()
			c.Deadline = c.IssuedAt.Add(time.Hour)
			raw, err := json.Marshal(c)
			if err != nil {
				return err
			}
			state := "outcome_unknown"
			if i == 129 {
				state = "queued"
				fresh = c
			}
			if _, err = tx.Exec("INSERT INTO commands(id,request_key,input_hash,producer,body,state,created) VALUES(?,?,?,?,?,?,?)", c.ID, c.RequestKey, c.InputHash, c.Producer.ID, string(raw), state, c.IssuedAt.UnixNano()); err != nil {
				return err
			}
			if i != 129 {
				if _, err = tx.Exec("INSERT INTO recovery_commands SELECT * FROM commands WHERE id=?", c.ID); err != nil {
					return err
				}
			}
		}
		return nil
	}))
	runtime := &Store{db: f.r.db, dir: f.r.dir, identity: f.r.journal.After, config: f.r.config, now: time.Now}
	first, err := runtime.Commands(ctx, producer)
	must(t, err)
	if len(first) != 0 {
		t.Fatal("historical page leaked", first)
	}
	second, err := runtime.Commands(ctx, producer)
	must(t, err)
	if len(second) != 1 || second[0].Command.ID != fresh.ID {
		t.Fatal("historical page starved new commands", second)
	}
	must(t, runtime.ImportCommand(ctx, producer, fresh.ID, protocol.Hash(fresh)))
}

func TestCommandRecoveryGuardRejectsMissingHistory(t *testing.T) {
	for _, mode := range []string{"marker", "snapshot", "retained"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := commandInstallFixture(t, false)
			var err error
			switch mode {
			case "marker":
				_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='recovery'")
			case "snapshot":
				_, err = f.r.db.Exec("DROP TABLE recovery_commands")
			case "retained":
				_, err = f.r.db.Exec("DROP TABLE reconciliation_command_records")
			}
			must(t, err)
			runtime := &Store{db: f.r.db, dir: f.r.dir, identity: f.r.journal.After, config: f.r.config, now: time.Now}
			if _, err = runtime.Commands(context.Background(), producer); err == nil {
				t.Fatal("damaged recovery history bypassed fence")
			}
		})
	}
}
