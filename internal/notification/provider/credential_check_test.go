package provider

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
)

func TestTelegramCredentialChecksDoNotAcceptRedirectedProfiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	token, chat := filepath.Join(dir, "token"), filepath.Join(dir, "chat")
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(token, "123:secret")
	write(chat, "456")
	p := Profile{Kind: "telegram", TokenFile: token, ChatFile: chat, Destination: TelegramIdentity("123:secret", "456")}
	if err := CheckTelegramProfile(p); err != nil {
		t.Fatal(err)
	}
	write(token, "123:rotated")
	if err := CheckTelegramProfile(p); err != nil {
		t.Fatal("same bot rotation changed destination", err)
	}
	write(token, "999:secret")
	if CheckTelegramProfile(p) == nil {
		t.Fatal("another bot accepted")
	}
	write(token, "123:secret")
	write(chat, "789")
	if CheckTelegramProfile(p) == nil {
		t.Fatal("another chat accepted")
	}
	d := interaction.Destination{ID: "conversation", Provider: "telegram", Account: "123", NativeID: "456", Generation: "one"}
	p.Conversation, p.Destination, p.ChatFile = &d, d.Fingerprint(), ""
	if err := CheckTelegramProfile(p); err != nil {
		t.Fatal("conversation unexpectedly required legacy chat file", err)
	}
	write(token, "123:invalid token")
	if CheckTelegramProfile(p) == nil || CheckTelegramAccount(TelegramAccount{Account: "123", TokenFile: token}) == nil {
		t.Fatal("malformed credential accepted")
	}
	write(token, "123:secret")
	if err := os.Chmod(token, 0644); err != nil {
		t.Fatal(err)
	}
	if CheckTelegramProfile(p) == nil {
		t.Fatal("public credential accepted")
	}
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(token, 0600); err != nil {
		t.Fatal(err)
	}
	if CheckTelegramProfile(p) == nil {
		t.Fatal("FIFO credential accepted")
	}
}
