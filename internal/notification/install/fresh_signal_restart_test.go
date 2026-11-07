package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestFreshSignalExplicitRestartPreservesOwnership(t *testing.T) {
	binary := freshSignalTestBinary(t)
	for _, boundary := range []string{"restart-intent", "restart-anchored", "restart-start-returned", "restart-verified", "restart-complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, f := freshSignalFixture(t, binary)
			i.restartProbe = func(context.Context, *safefs.Directory, FreshSignalManifest) error { return nil }
			before, err := i.setup(ctx, spec)
			testutil.Must(t, err)
			original, err := os.ReadFile(filepath.Join(spec.Root, "complete.json"))
			testutil.Must(t, err)
			for round := 0; round < 2; round++ {
				f.state.Active, f.state.PID = false, 0
				starts := f.starts
				if _, err = i.setup(ctx, spec); err == nil || f.starts != starts {
					t.Fatal("ordinary apply restarted stopped service")
				}
				stop := errors.New("restart interrupted")
				i.fault = func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				}
				if _, err = i.restart(ctx, before.reference()); !errors.Is(err, stop) {
					t.Fatal("missing boundary", boundary, err)
				}
				i.fault = nil
				for range 2 {
					got, e := i.restart(ctx, before.reference())
					testutil.Must(t, e)
					if protocol.Hash(got) != protocol.Hash(before) || f.starts != starts+1 {
						t.Fatal("restart replayed or changed ownership")
					}
				}
				d, err := safefs.Open(spec.Root)
				testutil.Must(t, err)
				j, err := readFreshSignalJournal(d, before)
				testutil.Must(t, err)
				r, gap, err := readFreshSignalRestart(d, before, j, false)
				testutil.Must(t, err)
				d.Close()
				if r == nil || !r.Complete || gap || round == 1 && r.Intent.Previous == "" {
					t.Fatal("restart history lost")
				}
				got, err := os.ReadFile(filepath.Join(spec.Root, "complete.json"))
				testutil.Must(t, err)
				if string(got) != string(original) {
					t.Fatal("restart replaced completed installation")
				}
				testutil.Must(t, signalaccount.InspectNativeService(spec.AccountState, before.binding()))
			}
		})
	}
}

func TestFreshSignalRestartRejectsDriftAndLostHistory(t *testing.T) {
	binary := freshSignalTestBinary(t)
	for _, damage := range []string{"unit", "disabled", "account-busy", "capability", "missing-completion", "missing-restart", "anchor", "history", "removal"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			spec, i, f := freshSignalFixture(t, binary)
			i.restartProbe = func(context.Context, *safefs.Directory, FreshSignalManifest) error { return nil }
			m, err := i.setup(ctx, spec)
			testutil.Must(t, err)
			f.state.Active, f.state.PID = false, 0
			d, err := safefs.Open(spec.Root)
			testutil.Must(t, err)
			defer d.Close()
			switch damage {
			case "unit":
				testutil.Must(t, os.WriteFile(filepath.Join(spec.UnitDirectory, freshSignalUnit), []byte("foreign"), 0600))
			case "disabled":
				f.state.Enabled = false
			case "account-busy":
				a, e := signalaccount.OpenNativeRuntime(spec.AccountState, m.binding(), func() error { return nil })
				testutil.Must(t, e)
				defer a.Close()
			case "capability":
				i.restartProbe = func(context.Context, *safefs.Directory, FreshSignalManifest) error { return errors.New("old helper") }
			case "missing-completion":
				testutil.Must(t, os.Remove(filepath.Join(spec.Root, "complete.json")))
			case "missing-restart", "anchor", "history":
				for range 2 {
					_, err = i.restart(ctx, m.reference())
					testutil.Must(t, err)
					f.state.Active, f.state.PID = false, 0
				}
				var r freshSignalRestart
				testutil.Must(t, d.ReadJSON("restart.json", &r))
				if damage == "missing-restart" {
					testutil.Must(t, os.Remove(filepath.Join(spec.Root, "restart.json")))
				} else if damage == "history" {
					testutil.Must(t, os.Remove(filepath.Join(spec.Root, "restart-history", r.Intent.Previous+".json")))
				} else {
					var j signalServiceJournal
					testutil.Must(t, d.ReadJSON("lifecycle.json", &j))
					j.Restart = ""
					testutil.Must(t, d.WriteJSON("lifecycle.json", j, true))
				}
			case "removal":
				testutil.Must(t, d.WriteJSON("remove.json", m.binding(), false))
			}
			starts := f.starts
			if _, err = i.restart(ctx, m.reference()); err == nil || f.starts != starts {
				t.Fatal("unsafe restart", err)
			}
		})
	}
}
