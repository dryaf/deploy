package workerctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/safefs"
)

type journalStep struct {
	name  string
	calls int
	id    string
	done  bool
	lost  bool
	drift bool
}

func (s *journalStep) Name() string { return s.name }
func (s *journalStep) Inspect(ctx context.Context, id string) (string, bool, error) {
	if s.id != "" && id != s.id {
		return "", false, fmt.Errorf("operation identity changed")
	}
	if s.drift {
		return "", false, fmt.Errorf("canonical evidence unavailable")
	}
	if s.done {
		return strings.Repeat("a", 64), true, nil
	}
	return "", false, nil
}
func (s *journalStep) Run(ctx context.Context, id string) error {
	s.calls++
	s.id = id
	s.done = true
	if s.lost {
		return errors.New("lost reply")
	}
	return nil
}
func TestCoordinatorResumesCanonicalChildAtEveryBoundary(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, boundary := range []string{"intent", "ran/enroll", "recorded/enroll", "ran/install", "recorded/install", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			d, err := safefs.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			a, b := &journalStep{name: "enroll"}, &journalStep{name: "install"}
			pins := OperationPins{Worker: "notification-worker", Action: "apply", DesiredHash: strings.Repeat("b", 64), UID: os.Getuid(), Home: root}
			coordinator := JournalCoordinator{Directory: d, Boundary: func(name string) error {
				if name == boundary {
					return errors.New("interrupted")
				}
				return nil
			}}
			first, err := coordinator.Apply(context.Background(), pins, []LifecycleStep{a, b})
			if err == nil {
				t.Fatal("fault not reached")
			}
			coordinator.Boundary = nil
			resumed, err := coordinator.Apply(context.Background(), pins, []LifecycleStep{a, b})
			if err != nil {
				t.Fatal(err)
			}
			if !resumed.Complete || resumed.ID != first.ID || a.calls != 1 || b.calls != 1 {
				t.Fatal("duplicate child operation", resumed, a.calls, b.calls)
			}
			if _, err = coordinator.Apply(context.Background(), pins, []LifecycleStep{a, b}); err != nil || a.calls != 1 || b.calls != 1 {
				t.Fatal("unchanged apply mutated", err)
			}
			pins.DesiredHash = strings.Repeat("c", 64)
			if _, err = coordinator.Apply(context.Background(), pins, []LifecycleStep{a, b}); err == nil {
				t.Fatal("changed YAML retargeted operation")
			}
		})
	}
}
func TestCoordinatorLostAckAndMissingEvidence(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	root := t.TempDir()
	os.Chmod(root, 0700)
	d, err := safefs.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	step := &journalStep{name: "install", lost: true}
	j := JournalCoordinator{Directory: d}
	pins := OperationPins{Worker: "backup-worker", Action: "apply", DesiredHash: strings.Repeat("a", 64), UID: os.Getuid(), Home: root}
	if _, err = j.Apply(context.Background(), pins, []LifecycleStep{step}); err == nil {
		t.Fatal("expected lost reply")
	}
	if _, err = j.Apply(context.Background(), pins, []LifecycleStep{step}); err != nil || step.calls != 1 {
		t.Fatal("lost acknowledgement repeated action", err)
	}
	step.drift = true
	if _, err = j.Apply(context.Background(), pins, []LifecycleStep{step}); err == nil {
		t.Fatal("missing evidence accepted")
	}
}

func TestCoordinatorNewPolicyRequiresCompletedPredecessor(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	root := t.TempDir()
	os.Chmod(root, 0700)
	d, err := safefs.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	j := JournalCoordinator{Directory: d}
	pins := OperationPins{Worker: "backup-worker", Action: "apply", DesiredHash: strings.Repeat("a", 64), UID: os.Getuid(), Home: root}
	old, err := j.Apply(context.Background(), pins, []LifecycleStep{&journalStep{name: "install"}})
	if err != nil {
		t.Fatal(err)
	}
	pins.DesiredHash = strings.Repeat("b", 64)
	pins.Previous = old.Digest()
	step := &journalStep{name: "update"}
	updated, err := j.Apply(context.Background(), pins, []LifecycleStep{step})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Complete || updated.ID == old.ID || step.calls != 1 {
		t.Fatal("update did not create one new operation")
	}
	var retained OperationJournal
	if err = d.ReadJSON("history/"+old.ID+".json", &retained); err != nil || retained.Digest() != old.Digest() {
		t.Fatal("predecessor lost", err)
	}
}

func TestCoordinatorMissingJournalCannotInitializeAgain(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	root := t.TempDir()
	os.Chmod(root, 0700)
	d, err := safefs.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	j := JournalCoordinator{Directory: d}
	pins := OperationPins{Worker: "backup-worker", Action: "apply", DesiredHash: strings.Repeat("a", 64), UID: os.Getuid(), Home: root}
	step := &journalStep{name: "install"}
	if _, err = j.Apply(context.Background(), pins, []LifecycleStep{step}); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(root + "/operation.json"); err != nil {
		t.Fatal(err)
	}
	if _, err = j.Apply(context.Background(), pins, []LifecycleStep{step}); err == nil || step.calls != 1 {
		t.Fatal("missing journal silently initialized", err)
	}
}
