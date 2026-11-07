package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestEnrolledPolicyAlternatesWithLEDMapping(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			ctx := context.Background()
			path, c, output, api := enrolledLimitsOutputFixture(t, disabled)
			original, err := os.ReadFile(filepath.Join(path, ledOutputFile))
			must(t, err)
			for round := 0; round < 3; round++ {
				after := c
				after.MaxMessages++
				op, err := safefs.NewID("n")
				must(t, err)
				stop := errors.New("lost policy SQL response")
				_, err = migrateEnrolledLimits(ctx, path, output.Identity, c, after, op, func(at string) error {
					if at == "committed" {
						return stop
					}
					return nil
				})
				if !errors.Is(err, stop) {
					t.Fatal(round, err)
				}
				_, err = MigrateEnrolledLimits(ctx, path, output.Identity, c, after, op)
				must(t, err)
				c = after
				after = c
				after.LEDBindings = append([]LEDBinding(nil), c.LEDBindings...)
				after.LEDBindings[0].Subjects = []string{fmt.Sprintf("source-%d", round)}
				op, err = safefs.NewID("n")
				must(t, err)
				_, err = MigrateManagedLEDBindings(ctx, path, c, after, migrationSocket, api, output.Identity, op)
				must(t, err)
				c = after
				s, err := OpenManagedVerification(path, c, output.Identity)
				must(t, err)
				must(t, s.RequireLEDOutput(ctx, output.Operation, disabled))
				must(t, s.Close())
			}
			after := c
			after.MaxMessages++
			op, err := safefs.NewID("n")
			must(t, err)
			_, err = MigrateEnrolledLimits(ctx, path, output.Identity, c, after, op)
			must(t, err)
			c = after
			got, err := os.ReadFile(filepath.Join(path, ledOutputFile))
			must(t, err)
			if string(got) != string(original) {
				t.Fatal("sequence rewrote output authority")
			}
			if _, ok := api.states["foreign"]; !ok {
				t.Fatal("sequence removed foreign output")
			}
			s, err := OpenManagedVerification(path, c, output.Identity)
			must(t, err)
			// Losing an intervening mapping cannot be repaired by adopting the latest
			// layout or trusting policy fields that happen to match.
			_, err = s.db.Exec("DELETE FROM led_migrations WHERE position=2")
			must(t, err)
			must(t, s.Close())
			if s, err = OpenManagedVerification(path, c, output.Identity); err == nil {
				s.Close()
				t.Fatal("missing intervening mapping admitted")
			}
		})
	}
}

func TestEnrolledPolicyRejectsUnprovedMappingGap(t *testing.T) {
	ctx := context.Background()
	path, c, output, _ := enrolledLimitsOutputFixture(t, false)
	after := c
	after.MaxMessages++
	op, err := safefs.NewID("n")
	must(t, err)
	_, err = MigrateEnrolledLimits(ctx, path, output.Identity, c, after, op)
	must(t, err)
	c = after
	d, err := safefs.Open(path)
	must(t, err)
	defer d.Close()
	var prior enrolledLimits
	must(t, d.ReadJSON(enrolledLimitsFile, &prior))
	// This is structurally a mapping, but no mapping transaction authorized it.
	before := c
	before.LEDBindings = append([]LEDBinding(nil), c.LEDBindings...)
	before.LEDBindings[0].Subjects = []string{"unreviewed"}
	after = before
	after.MaxMessages++
	op, err = safefs.NewID("n")
	must(t, err)
	if _, err = MigrateEnrolledLimits(ctx, path, output.Identity, before, after, op); err == nil {
		t.Fatal("unproved layout accepted")
	}
	var retained enrolledLimits
	must(t, d.ReadJSON(enrolledLimitsFile, &retained))
	if protocol.Hash(prior) != protocol.Hash(retained) {
		t.Fatal("refusal replaced policy authority")
	}
}
