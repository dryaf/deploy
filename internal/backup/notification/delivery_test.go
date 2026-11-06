package notification

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func TestPinnedTelegramSenderRefusesChangedRequestCredentials(t *testing.T) {
	policy := configfixture.WorkerPolicy(t).Notifications
	policy.Primary, policy.Fallback, policy.Signal = "telegram", "", nil
	policy.Telegram = &config.WorkerTelegram{}
	policy.Telegram.TokenFile = filepath.Join(t.TempDir(), "token")
	policy.Telegram.ChatIDFile = filepath.Join(t.TempDir(), "chat")
	testutil.Must(t, os.WriteFile(policy.Telegram.TokenFile, []byte("123:original"), 0600))
	testutil.Must(t, os.WriteFile(policy.Telegram.ChatIDFile, []byte("456"), 0600))
	pin, err := TelegramCredentialHash(policy)
	testutil.Must(t, err)
	// Mutate after admission. The request constructor must reject before HTTP.
	testutil.Must(t, os.WriteFile(policy.Telegram.ChatIDFile, []byte("789"), 0600))
	attempt, err := PinnedTelegramSender(pin)(context.Background(), "telegram", policy, NotificationEvent{ID: "test", Message: "fixture"})
	if err == nil || !strings.Contains(err.Error(), "credentials changed") || attempt.Accepted || attempt.Ambiguous {
		t.Fatal("changed target was not rejected before I/O", attempt, err)
	}
}
