//go:build notificationintegration

package install

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
)

func TestTelegramHandoffCheckNativeExecutable(t *testing.T) {
	flags, cursor := telegramCheckCLIArguments(t)
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	time.Sleep(time.Until(cursor.NotBefore.Add(10 * time.Millisecond)))
	args := append([]string{"notification-worker", "recovery-telegram-check"}, flags...)
	for attempt := 0; attempt < 2; attempt++ {
		out, err := process.LimitedOutput(exec.Command(binary, args...), time.Minute, 32768)
		if err != nil {
			t.Fatal(err, string(out))
		}
		var got worker.PublishedTelegramHandoff
		if err = json.Unmarshal(out, &got); err != nil {
			t.Fatal(err, string(out))
		}
		if got.Cursor != cursor || got.IntakeEnabled || !got.RequiresActivationRecheck {
			t.Fatal(got)
		}
	}
}

func TestTelegramRenewalCheckNativeExecutable(t *testing.T) {
	flags, renewal := telegramRenewalCheckCLIArguments(t)
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	time.Sleep(time.Until(renewal.Cursor.NotBefore.Add(10 * time.Millisecond)))
	args := append([]string{"notification-worker", "recovery-telegram-check"}, flags...)
	out, err := process.LimitedOutput(exec.Command(binary, args...), time.Minute, 32768)
	if err != nil {
		t.Fatal(err, string(out))
	}
	var got worker.PublishedTelegramHandoff
	if err = json.Unmarshal(out, &got); err != nil {
		t.Fatal(err, string(out))
	}
	if got.RenewalHash != renewal.RenewalHash || got.Cursor != renewal.Cursor || got.IntakeEnabled || !got.RequiresActivationRecheck {
		t.Fatal(got)
	}
	if out, err = process.LimitedOutput(exec.Command(binary, args[:len(args)-2]...), time.Minute, 32768); err == nil {
		t.Fatal("native original fallback accepted after renewal", string(out))
	}
}
