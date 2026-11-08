package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestCommandProducerDeclarationReviewIsReadOnlyAndChecksAuthority(t *testing.T) {
	ctx := context.Background()
	spec, i, runtime, active, qualification := managedCommandsFixture(t)
	i.commandProducerProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	enabled, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(qualification), "read", false)
	testutil.Must(t, err)
	s, err := openManagedStore(enabled, runtime)
	testutil.Must(t, err)
	testutil.Must(t, s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
	dst := runtime.Worker.Interaction.Destinations[0]
	pending, err := s.AdmitCommand(ctx, interaction.Inbound{Provider: "telegram", Account: "123", NativeID: "declaration-request", Actor: "789", Conversation: dst.ID, DestinationFingerprint: dst.Fingerprint(), SentAt: time.Now().UTC(), Text: "worker/local status"})
	testutil.Must(t, err)
	testutil.Must(t, s.Close())
	disabled, err := i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(enabled), "", "", false)
	testutil.Must(t, err)
	raw, err := os.ReadFile(spec.RuntimeFile)
	testutil.Must(t, err)
	lifecycle, err := os.ReadFile(filepath.Join(spec.Root, "lifecycle.json"))
	testutil.Must(t, err)
	i.commandPauseProbe = func(context.Context, *safefs.Directory, string, string) error {
		t.Fatal("equivalent declaration probed pause")
		return nil
	}
	for range 2 {
		got, err := i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(disabled), raw, "command-declaration")
		testutil.Must(t, err)
		if protocol.Hash(got.Before) != protocol.Hash(disabled) {
			t.Fatal("review changed gateway")
		}
	}
	after, err := os.ReadFile(filepath.Join(spec.Root, "lifecycle.json"))
	testutil.Must(t, err)
	if string(lifecycle) != string(after) {
		t.Fatal("declaration review wrote lifecycle intent")
	}
	s, err = openManagedStore(disabled, runtime)
	testutil.Must(t, err)
	commands, err := s.Commands(ctx, runtime.Worker.Producers[0].Producer)
	testutil.Must(t, err)
	testutil.Must(t, s.Close())
	if len(commands) != 1 || !reflect.DeepEqual(commands[0], pending) {
		t.Fatal("declaration changed pending request")
	}
	if _, err = i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(enabled), raw, "command-declaration"); err == nil {
		t.Fatal("review accepted stale enabled manifest")
	}
	var input RuntimeInput
	testutil.Must(t, safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes))
	input.Worker.Producers[0].MaxMessages++
	changed, err := input.Bytes()
	testutil.Must(t, err)
	if _, err = i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(disabled), changed, "command-declaration"); err == nil {
		t.Fatal("declaration changed effective producer policy")
	}
	i.commandProducerProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("old helper") }
	if _, err = i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(disabled), raw, "command-declaration"); err == nil {
		t.Fatal("declaration ignored missing capability")
	}
}
