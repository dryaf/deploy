package provider

import (
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
)

func TestSignalNativeIdentityIgnoresDisplayNamesAndPinsContent(t *testing.T) {
	account := "11111111-1111-4111-8111-111111111111"
	actor := "22222222-2222-4222-8222-222222222222"
	d := interaction.Destination{ID: "private", Generation: "one", Provider: "signal", Account: account, NativeID: actor}
	raw := `{"account":"+10000000000","envelope":{"sourceUuid":"22222222-2222-4222-8222-222222222222","sourceName":"Alice","sourceDevice":1,"timestamp":1793443200000,"dataMessage":{"timestamp":1793443200000,"message":"backup/storage status"}}}`
	first, err := NormalizeSignal([]byte(raw), "+10000000000", account, []interaction.Destination{d})
	if err != nil || first.Inbound == nil {
		t.Fatal(first, err)
	}
	second, err := NormalizeSignal([]byte(strings.ReplaceAll(raw, "Alice", "Renamed")), "+10000000000", account, []interaction.Destination{d})
	if err != nil || first.Key != second.Key || first.Hash != second.Hash {
		t.Fatal("profile rename changed request identity", err)
	}
	changed, err := NormalizeSignal([]byte(strings.ReplaceAll(raw, "status", "retry")), "+10000000000", account, []interaction.Destination{d})
	if err != nil || changed.Key != first.Key || changed.Hash == first.Hash {
		t.Fatal("payload conflict not detectable", err)
	}
	for _, value := range []string{strings.ReplaceAll(raw, actor, account), strings.ReplaceAll(raw, `"sourceUuid"`, `"sourceName"`), strings.ReplaceAll(raw, `"dataMessage"`, `"syncMessage"`), strings.ReplaceAll(raw, `"message":"backup/storage status"`, `"message":"backup/storage status","isEndSession":true`)} {
		e, err := NormalizeSignal([]byte(value), "+10000000000", account, []interaction.Destination{d})
		if err == nil && e.Inbound != nil {
			t.Fatal("unsupported Signal event became a command")
		}
	}
}
