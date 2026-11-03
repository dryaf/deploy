package worker

import (
	"context"
	"testing"
)

func TestRecoveryHandoffRequiresActualCommandAbsence(t *testing.T) {
	for _, kind := range []string{"current", "restored", "session", "records", "installation", "lost-marker", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			f, _, _ := generationFixture(t)
			ctx := context.Background()
			must(t, f.r.RequireCommandRecoveryAbsent(ctx, producer))
			var err error
			switch kind {
			case "current", "restored", "foreign":
				id := producer.ID
				if kind == "foreign" {
					id = "another-producer"
				}
				_, err = f.r.db.Exec("INSERT INTO commands(id,request_key,input_hash,producer,body,state,created) VALUES('retained-command','retained-request','hash',?,'{}','outcome_unknown',1)", id)
				must(t, err)
				if kind == "restored" {
					_, err = f.r.db.Exec("INSERT INTO recovery_commands SELECT * FROM commands; DELETE FROM commands")
				}
			case "session", "records", "lost-marker":
				must(t, f.r.initializeCommandRecovery(ctx))
				if kind == "session" {
					_, err = f.r.db.Exec("INSERT INTO reconciliation_command_sessions VALUES(?,'{}')", producer.ID)
				}
				if kind == "records" {
					_, err = f.r.db.Exec("INSERT INTO reconciliation_command_records VALUES(?,1,'command','request','hash','{}','outcome_unknown')", producer.ID)
				}
				if kind == "lost-marker" {
					_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='command_reconciliation_schema'")
				}
			case "installation":
				must(t, f.r.initializeCommandInstallations(ctx))
				_, err = f.r.db.Exec("INSERT INTO reconciliation_command_installations VALUES(?,'hash','{}')", producer.ID)
			}
			must(t, err)
			preflight := f.r.RequireCommandRecoveryAbsent(ctx, producer)
			_, commit := f.r.PrepareRecoveryProducerCommit(ctx, producer)
			if kind == "foreign" {
				must(t, preflight)
				must(t, commit)
			} else if preflight == nil || commit == nil {
				t.Fatal("missing command enrollment hid retained history", preflight, commit)
			}
		})
	}
}
