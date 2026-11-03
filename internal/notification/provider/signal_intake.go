package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

var signalUUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func ValidSignalAccount(id string) bool { return signalUUID.MatchString(id) }

// NormalizeSignal accepts the pinned 0.14.9 CLI receive envelope. It never uses
// display names or telephone numbers as actor authority. Sync messages,
// edits and attachment-only events do not become commands in this staging path.
func NormalizeSignal(raw []byte, selector, account string, destinations []interaction.Destination) (event interaction.SpoolEvent, err error) {
	if len(raw) > 32<<10 || !utf8.Valid(raw) || safefs.RejectDuplicateJSONKeys(raw) != nil || !signalUUID.MatchString(account) {
		return event, fmt.Errorf("invalid Signal envelope")
	}
	var wire struct {
		Account   string          `json:"account"`
		Exception json.RawMessage `json:"exception"`
		Envelope  *struct {
			Actor     string          `json:"sourceUuid"`
			Device    int             `json:"sourceDevice"`
			Timestamp int64           `json:"timestamp"`
			Sync      json.RawMessage `json:"syncMessage"`
			Edit      json.RawMessage `json:"editMessage"`
			Data      *struct {
				Timestamp        int64             `json:"timestamp"`
				Message          string            `json:"message"`
				Quote            json.RawMessage   `json:"quote"`
				ViewOnce         bool              `json:"viewOnce"`
				EndSession       bool              `json:"isEndSession"`
				ExpirationUpdate bool              `json:"isExpirationUpdate"`
				ProfileUpdate    bool              `json:"isProfileKeyUpdate"`
				Reaction         json.RawMessage   `json:"reaction"`
				Delete           json.RawMessage   `json:"remoteDelete"`
				Attachments      []json.RawMessage `json:"attachments"`
				Story            json.RawMessage   `json:"storyContext"`
				Group            *struct {
					ID   string `json:"groupId"`
					Type string `json:"type"`
				} `json:"groupInfo"`
			} `json:"dataMessage"`
		} `json:"envelope"`
	}
	if json.Unmarshal(raw, &wire) != nil || wire.Account != selector {
		return event, fmt.Errorf("Signal account mismatch")
	}
	event.Hash = protocol.Hash(json.RawMessage(raw))
	event.Key = "unsupported-" + event.Hash
	e := wire.Envelope
	if len(wire.Exception) != 0 || e == nil || !signalUUID.MatchString(e.Actor) || e.Actor == account || e.Device < 1 || e.Timestamp <= 0 || len(e.Sync) != 0 || len(e.Edit) != 0 || e.Data == nil {
		return event, nil
	}
	dm := e.Data
	conversation := e.Actor
	if dm.Group != nil {
		conversation = "group:" + dm.Group.ID
	}
	event.Key = protocol.Hash([]string{"signal-event-v1", account, e.Actor, strconv.Itoa(e.Device), conversation, strconv.FormatInt(e.Timestamp, 10), strconv.FormatInt(dm.Timestamp, 10)})
	// Exclude delivery timestamps and profile/display-name changes from the
	// immutable payload digest; a replay can legitimately change those metadata.
	event.Hash = protocol.Hash(struct {
		Key  string
		Data any
	}{event.Key, dm})
	if dm.Timestamp <= 0 || dm.ViewOnce || dm.EndSession || dm.ExpirationUpdate || dm.ProfileUpdate || len(dm.Reaction) > 0 || len(dm.Delete) > 0 || len(dm.Attachments) > 0 || len(dm.Story) > 0 || len(dm.Message) == 0 || len(dm.Message) > 4096 {
		return event, nil
	}
	for _, d := range destinations {
		if d.Provider != "signal" || d.Account != account || d.NativeID != conversation || d.Thread != "" {
			continue
		}
		if dm.Group != nil && dm.Group.Type != "DELIVER" {
			return event, nil
		}
		event.Inbound = &interaction.Inbound{Provider: "signal", Account: account, NativeID: event.Key, Actor: e.Actor, Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.UnixMilli(dm.Timestamp).UTC(), Text: dm.Message}
		if len(dm.Quote) > 0 {
			var quote struct {
				ID     int64  `json:"id"`
				Author string `json:"authorUuid"`
			}
			if json.Unmarshal(dm.Quote, &quote) != nil || quote.ID <= 0 || quote.Author != account {
				event.Inbound = nil
				break
			}
			event.Inbound.Reply = &interaction.NativeReply{ID: strconv.FormatInt(quote.ID, 10), Sender: quote.Author}
		}
		break
	}
	return
}
