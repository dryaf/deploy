package workerctl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/safefs"
)

func TestTargetReferencedCredentialRotationRequiresReview(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless paths")
	}
	r, paths := bootstrapControlFixture(t)
	p := r.Desired.Notification.Providers["telegram"]
	p.TokenInput, p.ChatIDInput = "", ""
	p.TokenFile = filepath.Join(r.Facts.Home, "private/token")
	p.ChatIDFile = filepath.Join(r.Facts.Home, "private/chat")
	r.Desired.Notification.Providers["telegram"] = p
	if err := os.MkdirAll(filepath.Dir(p.TokenFile), 0700); err != nil {
		t.Fatal(err)
	}
	for path, raw := range map[string][]byte{p.TokenFile: r.Inputs["token"], p.ChatIDFile: r.Inputs["chat_id"]} {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	input, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ensureConfigDirectory(paths.Control)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	op, _ := safefs.NewID("n")
	s := notificationBootstrapStep{paths: paths, input: input, secrets: r.Inputs}
	if err = s.pinInputs(op, true); err != nil {
		t.Fatal(err)
	}
	raw, err := input.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err = publishPrivateInput(paths.Runtime, raw); err != nil {
		t.Fatal(err)
	}
	if err = s.verifyFiles(); err != nil {
		t.Fatal(err)
	}
	rotated := []byte("123:rotated-same-account-token")
	if err = os.WriteFile(p.TokenFile, rotated, 0600); err != nil {
		t.Fatal(err)
	}
	s.secrets["token"] = rotated
	// The resolved provider identity remains the same bot/chat. Private pins
	// still refuse to call this credential change an unchanged installation.
	s.input, err = CompileUnboundNotification(r.Desired, paths, r.Facts.Home, s.secrets)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.verifyFiles(); err == nil {
		t.Fatal("target token rotation was ignored")
	}
	if err = os.Remove(filepath.Join(paths.Control, "notification-inputs.json")); err != nil {
		t.Fatal(err)
	}
	if err = s.pinInputs(op, true); err == nil {
		t.Fatal("lost credential pins silently recreated")
	}
}
