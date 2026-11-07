package workerctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

func changeDeclaredTelegramFiles(t *testing.T, r *ControlRequest, paths ResolvedPaths, audience bool) {
	t.Helper()
	raw, err := readLimitsRuntime(paths.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		t.Fatal(err)
	}
	p := input.Providers["telegram"]
	token, err := ReadCredentialInput(p.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := ReadCredentialInput(p.ChatFile)
	if err != nil {
		t.Fatal(err)
	}
	bot, _, _ := strings.Cut(strings.TrimSpace(string(token)), ":")
	token = []byte(bot + ":rotated_fixture")
	if audience {
		chat = []byte("987654321")
	}
	next := r.Desired.Notification.Providers["telegram"]
	next.TokenFile, next.ChatIDFile = filepath.Join(t.TempDir(), "token"), filepath.Join(t.TempDir(), "chat")
	next.TokenInput, next.ChatIDInput = "", ""
	if err = os.WriteFile(next.TokenFile, token, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(next.ChatIDFile, chat, 0600); err != nil {
		t.Fatal(err)
	}
	r.Desired.Notification.Providers["telegram"] = next
	r.Inputs = nil
}

func TestNotificationProvidersLostRepliesAndFollowingLifecycles(t *testing.T) {
	testNotificationPolicyLostReplies(t, "providers")
}

func TestNotificationProvidersRefusesUnreviewedChanges(t *testing.T) {
	for _, damage := range []string{"old-secret", "new-secret-mode", "bound", "commands", "mixed", "local-input", "missing-pins", "missing-snapshot", "in-place"} {
		t.Run(damage, func(t *testing.T) {
			r, paths, o, d := notificationLimitsFixture(t)
			oldRuntime, err := readLimitsRuntime(paths.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			var old install.RuntimeInput
			if err = safefs.DecodeJSON(oldRuntime, &old, maxControlFrame); err != nil {
				t.Fatal(err)
			}
			changeDeclaredTelegramFiles(t, &r, paths, true)
			switch damage {
			case "old-secret":
				err = os.WriteFile(old.Providers["telegram"].TokenFile, []byte("123:drift"), 0600)
			case "new-secret-mode":
				err = os.Chmod(r.Desired.Notification.Providers["telegram"].TokenFile, 0644)
			case "bound":
				o.Unbound = false
			case "commands":
				o.Manifest.CommandHistoryHash = strings.Repeat("a", 64)
			case "mixed":
				r.Desired.Notification.Limits.MaxMessages++
			case "local-input":
				p := r.Desired.Notification.Providers["telegram"]
				p.TokenInput, p.TokenFile = p.TokenFile, ""
				r.Desired.Notification.Providers["telegram"] = p
				r.Inputs = map[string][]byte{"token": []byte("123:secret")}
			case "missing-pins":
				err = os.Remove(filepath.Join(paths.Control, "notification-inputs.json"))
			case "missing-snapshot":
				var j OperationJournal
				if err = d.ReadJSON("operation.json", &j); err != nil {
					t.Fatal(err)
				}
				err = os.Remove(filepath.Join(paths.Control, "declarations", j.Pins.DesiredHash+".json"))
			case "in-place":
				p := r.Desired.Notification.Providers["telegram"]
				p.TokenFile = old.Providers["telegram"].TokenFile
				r.Desired.Notification.Providers["telegram"] = p
				err = os.WriteFile(p.TokenFile, []byte("123:replaced"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			r.DesiredHash, _ = r.Desired.Fingerprint()
			journal, err := os.ReadFile(filepath.Join(paths.Control, "operation.json"))
			if err != nil {
				t.Fatal(err)
			}
			l := notificationLimitsLifecycle{inspect: func(context.Context, string) (install.InstallationObservation, error) { return *o, nil }, migrate: func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error) {
				t.Fatal("unreviewed migration")
				return install.ManagedManifest{}, nil
			}}
			if _, handled, err := reconcileNotificationProviders(context.Background(), r, paths, "/helper", l); !handled || err == nil {
				t.Fatal("unsafe provider update accepted", err)
			}
			actual, err := os.ReadFile(filepath.Join(paths.Control, "operation.json"))
			if err != nil || string(actual) != string(journal) {
				t.Fatal("rejection changed authority", err)
			}
		})
	}
}
