package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestBootstrapLimitsRetainsReservationAndEnrollsRevisedPolicy(t *testing.T) {
	ctx := context.Background()
	path, op := bootstrapTarget(t)
	c := bootstrapConfig()
	b, err := BootstrapGateway(ctx, path, op, c)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(path, "bootstrap.json"))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		after := c
		after.MaxMessages += 10
		after.ReceiptAuditDays = 180
		change, _ := safefs.NewID("n")
		r, err := MigrateBootstrapLimits(ctx, path, b.Identity, c, after, change)
		if err != nil {
			t.Fatal(err)
		}
		if r.Identity != b.Identity {
			t.Fatal("changed identity")
		}
		if again, err := MigrateBootstrapLimits(ctx, path, b.Identity, c, after, change); err != nil || again.Operation != r.Operation {
			t.Fatal("repeat", err)
		}
		if s, err := OpenBootstrapVerification(path, c, b.Identity, ""); err == nil {
			s.Close()
			t.Fatal("old limits accepted")
		}
		c = after
	}
	got, _ := os.ReadFile(filepath.Join(path, "bootstrap.json"))
	if string(got) != string(original) {
		t.Fatal("rewrote reservation")
	}
	after := bootstrapConsumerConfig(c)
	change, _ := safefs.NewID("n")
	if _, err = EnrollBootstrap(ctx, path, b.Identity, after, change); err != nil {
		t.Fatal("enrollment after limits migration", err)
	}
	s, err := OpenManagedVerification(path, after, b.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RequireInactiveProducers(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err = os.Remove(filepath.Join(path, bootstrapLimitsFile)); err != nil {
		t.Fatal(err)
	}
	if s, err = OpenManagedVerification(path, after, b.Identity); err == nil {
		s.Close()
		t.Fatal("missing limits authority accepted after enrollment")
	}
}

func TestBootstrapLimitsResumeFencesOldAndNewPolicy(t *testing.T) {
	for _, boundary := range []string{"intent", "committed", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			path, op := bootstrapTarget(t)
			before := bootstrapConfig()
			b, err := BootstrapGateway(ctx, path, op, before)
			if err != nil {
				t.Fatal(err)
			}
			after := before
			after.MaxMessages += 10
			change, _ := safefs.NewID("n")
			stop := errors.New("lost response")
			r, err := migrateBootstrapLimits(ctx, path, b.Identity, before, after, change, func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if boundary != "complete" {
				for _, c := range []Config{before, after} {
					if s, e := OpenBootstrapVerification(path, c, b.Identity, ""); e == nil {
						s.Close()
						t.Fatal("pending limits admitted runtime")
					}
				}
			}
			wrong := after
			wrong.MaxMessages++
			if _, e := MigrateBootstrapLimits(ctx, path, b.Identity, before, wrong, change); e == nil {
				t.Fatal("retargeted migration")
			}
			again, err := MigrateBootstrapLimits(ctx, path, b.Identity, before, after, change)
			if err != nil || protocol.Hash(again) != protocol.Hash(r) {
				t.Fatal("resume", err)
			}
			s, err := OpenBootstrapVerification(path, after, b.Identity, "")
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
		})
	}
}

func TestBootstrapLimitsRejectsInvalidReductionAndMissingAuthority(t *testing.T) {
	for _, kind := range []string{"route", "database", "reserve", "history", "sql", "missing-file"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path, op := bootstrapTarget(t)
			before := bootstrapConfig()
			b, err := BootstrapGateway(ctx, path, op, before)
			if err != nil {
				t.Fatal(err)
			}
			after := before
			after.MaxMessages++
			change, _ := safefs.NewID("n")
			if kind == "route" {
				after.Routes = map[string]Route{"alerts": {Revision: "new", Primary: "telegram"}}
			}
			if kind == "reserve" {
				after.MinFreeBytes = 1 << 62
			}
			if kind == "database" {
				s, e := OpenBootstrapVerification(path, before, b.Identity, "")
				if e != nil {
					t.Fatal(e)
				}
				_, e = s.db.Exec("CREATE TABLE test_large(data BLOB); INSERT INTO test_large VALUES(zeroblob(?))", 2<<20)
				s.Close()
				if e != nil {
					t.Fatal(e)
				}
				after.MaxDatabaseBytes = 1 << 20
			}
			if kind == "route" || kind == "database" || kind == "reserve" {
				if _, err = MigrateBootstrapLimits(ctx, path, b.Identity, before, after, change); err == nil {
					t.Fatal("invalid limits accepted")
				}
				if _, err = os.Stat(filepath.Join(path, bootstrapLimitsFile)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("invalid change retained intent", err)
				}
				return
			}
			if _, err = MigrateBootstrapLimits(ctx, path, b.Identity, before, after, change); err != nil {
				t.Fatal(err)
			}
			if kind == "sql" {
				s, e := OpenBootstrapVerification(path, after, b.Identity, "")
				if e != nil {
					t.Fatal(e)
				}
				_, e = s.db.Exec("DELETE FROM metadata WHERE key=?", bootstrapLimitsKey)
				s.Close()
				if e != nil {
					t.Fatal(e)
				}
			} else if kind == "missing-file" {
				if err = os.Remove(filepath.Join(path, bootstrapLimitsFile)); err != nil {
					t.Fatal(err)
				}
			} else {
				next := after
				next.MaxMessages++
				change, _ = safefs.NewID("n")
				if _, err = MigrateBootstrapLimits(ctx, path, b.Identity, after, next, change); err != nil {
					t.Fatal(err)
				}
				after = next
				entries, e := os.ReadDir(filepath.Join(path, "bootstrap-limits-history"))
				if e != nil || len(entries) != 1 {
					t.Fatal(entries, e)
				}
				if err = os.Remove(filepath.Join(path, "bootstrap-limits-history", entries[0].Name())); err != nil {
					t.Fatal(err)
				}
			}
			if s, e := OpenBootstrapVerification(path, after, b.Identity, ""); e == nil {
				s.Close()
				t.Fatal("missing authority accepted")
			}
		})
	}
}
