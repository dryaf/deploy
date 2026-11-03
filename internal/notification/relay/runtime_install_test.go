package relay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

var runtimeInstallBoundaries = []string{"runtime_source_fenced", "runtime_destination_intent", "runtime_image_copy", "runtime_image_transaction", "runtime_image_promoted", "runtime_image_sealed", "runtime_database_installed", "runtime_identity_installed", "runtime_destination_complete", "runtime_source_complete"}

func runtimeInstallFixture(t *testing.T, restored bool) (string, string, interaction.SignalRecoveryCursor) {
	t.Helper()
	source, path, handoff := cursorRelayFixture(t, restored)
	proof, err := source.StageSignalCursor(context.Background(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err = os.Chmod(dst, 0700); err != nil {
		t.Fatal(err)
	}
	return path, dst, proof
}
func TestSignalRuntimeInstallVerification(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprint(restored), func(t *testing.T) {
			src, dst, proof := runtimeInstallFixture(t, restored)
			ctx := context.Background()
			before, err := os.ReadFile(filepath.Join(src, "inbox.db"))
			if err != nil {
				t.Fatal(err)
			}
			installed, err := InstallSignalRuntime(ctx, src, dst, proof)
			if err != nil {
				t.Fatal(err)
			}
			if installed.IntakeEnabled || installed.InstallationHash == "" {
				t.Fatal(installed)
			}
			again, err := InstallSignalRuntime(ctx, src, dst, proof)
			if err != nil {
				t.Fatal(err)
			}
			if protocol.Hash(again) != protocol.Hash(installed) {
				t.Fatal("installation replay changed")
			}
			after, err := os.ReadFile(filepath.Join(src, "inbox.db"))
			if err != nil || string(before) != string(after) {
				t.Fatal("source database changed", err)
			}
			for _, path := range []string{src, dst} {
				if s, e := OpenInbox(path, proof.Handoff.Target.Account, false); e == nil {
					s.Close()
					t.Fatal("normal startup bypassed installation")
				}
			}
			for n := 0; n < 2; n++ {
				s, e := OpenInstalledInboxVerification(ctx, dst, installed.InstallationHash)
				if e != nil {
					t.Fatal(e)
				}
				if s.Cursor() != proof.Handoff.Target {
					t.Fatal(s.Cursor())
				}
				for _, e := range []error{s.CheckActive(ctx), s.CanReceive(ctx), s.Enqueue(ctx, interaction.SpoolEvent{Key: "new", Hash: protocol.Hash("new")}), s.Ack(ctx, s.Cursor())} {
					if e == nil {
						t.Fatal("verification granted intake")
					}
				}
				if _, e = s.List(ctx, s.Cursor()); e == nil {
					t.Fatal("verification listed provider messages")
				}
				if e = s.Close(); e != nil {
					t.Fatal(e)
				}
			}
			if _, err = VerifyInstalledSignalRuntime(ctx, dst, installed.InstallationHash); err != nil {
				t.Fatal(err)
			}
			if _, err = InstallSignalRuntime(ctx, src, dst, proof); err != nil {
				t.Fatal("retry after WAL startup", err)
			}
		})
	}
}
func TestSignalRuntimeInstallInterrupted(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, point := range runtimeInstallBoundaries {
			t.Run(fmt.Sprintf("%t/%s", restored, point), func(t *testing.T) {
				src, dst, p := runtimeInstallFixture(t, restored)
				stop := errors.New("interrupted")
				_, err := installSignalRuntime(context.Background(), src, dst, p, func(at string) error {
					if at == point {
						return stop
					}
					return nil
				})
				if !errors.Is(err, stop) {
					t.Fatal(err)
				}
				if s, e := OpenInbox(dst, p.Handoff.Target.Account, false); e == nil {
					s.Close()
					t.Fatal("interruption enabled intake")
				}
				installed, err := InstallSignalRuntime(context.Background(), src, dst, p)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = VerifyInstalledSignalRuntime(context.Background(), dst, installed.InstallationHash); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
func TestSignalRuntimeInstallRejectsChangedEvidence(t *testing.T) {
	for _, kind := range []string{"source-intent", "source-complete", "target-intent", "target-complete", "seal", "image", "baseline", "database", "lease", "candidate", "foreign-destination", "destination-replaced"} {
		t.Run(kind, func(t *testing.T) {
			src, dst, p := runtimeInstallFixture(t, false)
			ctx := context.Background()
			installed, err := InstallSignalRuntime(ctx, src, dst, p)
			if err != nil {
				t.Fatal(err)
			}
			var name string
			switch kind {
			case "source-intent":
				name = filepath.Join(src, runtimeInstallIntent)
			case "source-complete":
				name = filepath.Join(src, runtimeInstallComplete)
			case "target-intent":
				name = filepath.Join(dst, runtimeInstallIntent)
			case "target-complete":
				name = filepath.Join(dst, runtimeInstallComplete)
			case "seal":
				name = filepath.Join(dst, runtimeInstallSeal)
			case "image":
				name = filepath.Join(dst, runtimeInstallImage)
			case "baseline":
				name = filepath.Join(dst, signalRuntimeFile)
			case "database":
				name = filepath.Join(dst, "inbox.db")
			case "lease":
				name = filepath.Join(dst, "inbox.lock")
			case "candidate":
				name = filepath.Join(src, signalCursorDatabase)
			case "foreign-destination":
				other := t.TempDir()
				if err = os.Chmod(other, 0700); err != nil {
					t.Fatal(err)
				}
				if _, err = InstallSignalRuntime(ctx, src, other, p); err == nil {
					t.Fatal("source installed twice")
				}
				return
			case "destination-replaced":
				if err = os.Rename(dst, dst+"-retained"); err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(dst + "-retained")
				if err = os.Mkdir(dst, 0700); err != nil {
					t.Fatal(err)
				}
				if _, err = InstallSignalRuntime(ctx, src, dst, p); err == nil {
					t.Fatal("replaced destination accepted")
				}
				return
			}
			if err = os.Remove(name); err != nil {
				t.Fatal(err)
			}
			if s, e := OpenInstalledInboxVerification(ctx, dst, installed.InstallationHash); e == nil {
				s.Close()
				t.Fatal("accepted missing " + kind)
			}
		})
	}
}
func TestSignalRuntimeInstallUnownedAndLockedDestination(t *testing.T) {
	for _, kind := range []string{"nonempty", "symlink", "nested", "locked", "wrong-proof"} {
		t.Run(kind, func(t *testing.T) {
			src, dst, p := runtimeInstallFixture(t, false)
			switch kind {
			case "nonempty":
				if err := os.WriteFile(filepath.Join(dst, "keep"), []byte("operator"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(src, "inbox.db"), filepath.Join(dst, "inbox.db")); err != nil {
					t.Fatal(err)
				}
			case "nested":
				dst = filepath.Join(src, "target")
				if err := os.Mkdir(dst, 0700); err != nil {
					t.Fatal(err)
				}
			case "locked":
				d, err := safefs.Open(src)
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				l, err := d.Lock("inbox.lock", true, false)
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
			case "wrong-proof":
				p.DatabaseHash = protocol.Hash("wrong")
			}
			if _, err := InstallSignalRuntime(context.Background(), src, dst, p); err == nil {
				t.Fatal("accepted " + kind)
			}
			if _, err := os.Stat(filepath.Join(src, runtimeInstallIntent)); !os.IsNotExist(err) {
				t.Fatal("invalid preflight fenced source", err)
			}
		})
	}
}

func TestSignalRuntimeInstallRejectsBoundaryDrift(t *testing.T) {
	for _, name := range []string{runtimeInstallSeal, runtimeInstallIntent, "inbox.lock", runtimeInstallImage} {
		t.Run(name, func(t *testing.T) {
			src, dst, p := runtimeInstallFixture(t, false)
			_, err := installSignalRuntime(context.Background(), src, dst, p, func(at string) error {
				if at == "runtime_image_sealed" {
					return os.Remove(filepath.Join(dst, name))
				}
				return nil
			})
			if err == nil {
				t.Fatal("changed installation accepted")
			}
			if _, err = os.Stat(filepath.Join(src, runtimeInstallComplete)); !os.IsNotExist(err) {
				t.Fatal("drift published source completion", err)
			}
		})
	}
}
func TestSignalRuntimeInstallLostCompletionAfterVerification(t *testing.T) {
	src, dst, p := runtimeInstallFixture(t, false)
	ctx := context.Background()
	installed, err := InstallSignalRuntime(ctx, src, dst, p)
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenInstalledInboxVerification(ctx, dst, installed.InstallationHash)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(src, runtimeInstallComplete)); err != nil {
		t.Fatal(err)
	}
	if _, err = InstallSignalRuntime(ctx, src, dst, p); err == nil {
		t.Fatal("lost source completion reconstructed after runtime open")
	}
}
func TestSignalRuntimeInstallRetainsPriorBaseline(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprint(restored), func(t *testing.T) {
			source, path, handoff := baselineHandoffRelayFixture(t, restored)
			cursorSource := source.(interaction.SignalCursorSource)
			proof, err := cursorSource.StageSignalCursor(context.Background(), handoff)
			if err != nil {
				t.Fatal(err)
			}
			if err = source.Close(); err != nil {
				t.Fatal(err)
			}
			dst := t.TempDir()
			if err = os.Chmod(dst, 0700); err != nil {
				t.Fatal(err)
			}
			installed, err := InstallSignalRuntime(context.Background(), path, dst, proof)
			if err != nil {
				t.Fatal(err)
			}
			if installed.Cursor.Handoff.Summary.Manifest.BaselineHash == "" {
				t.Fatal("fixture lacks inherited baseline")
			}
			verified, err := OpenInstalledInboxVerification(context.Background(), dst, installed.InstallationHash)
			if err != nil {
				t.Fatal(err)
			}
			if verified.Cursor() != proof.Handoff.Target {
				t.Fatal("baseline changed")
			}
			if err = verified.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
