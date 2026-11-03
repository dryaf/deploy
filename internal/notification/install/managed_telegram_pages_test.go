package install

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type managedTelegramPageTransport struct {
	offsets   []int64
	afterPage func()
}

func (p *managedTelegramPageTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(r.URL.Path, "/getUpdates") {
		return (telegramCheckTransport{}).RoundTrip(r)
	}
	var request struct {
		Offset int64 `json:"offset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return nil, err
	}
	p.offsets = append(p.offsets, request.Offset)
	body := `{"ok":true,"result":[]}`
	if request.Offset == 0 {
		// Unsupported input still requires durable evidence before its ACK.
		body = `{"ok":true,"result":[{"update_id":100,"edited_message":{"text":"discard"}}]}`
		if p.afterPage != nil {
			p.afterPage()
		}
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func TestManagedTelegramPageGuard(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "acknowledged"
		if changed {
			name = "service-restarted-during-response"
		}
		t.Run(name, func(t *testing.T) {
			spec, i, f, _ := managedTelegramFixture(t, nil)
			p := &managedTelegramPageTransport{}
			i.telegramTransport = p
			if changed {
				p.afterPage = func() { f.state.Active = "active" }
			}
			starts := f.starts
			m, err := i.prepareTelegramRenewal(context.Background(), spec.Root)
			if changed {
				if err == nil || len(p.offsets) != 1 || p.offsets[0] != 0 || f.starts != starts {
					t.Fatal("changed authority allowed ACK/restart", err, p.offsets)
				}
				p.afterPage = nil
				p.offsets = nil
				m, err = i.resume(context.Background(), spec.Root)
			}
			if err != nil {
				t.Fatal(err)
			}
			if protocol.Hash(p.offsets) != protocol.Hash([]int64{0, 101}) {
				t.Fatal("incorrect retry/ACK offsets", p.offsets)
			}
			receipt := managedTelegramReceiptForTest(t, m)
			if receipt.Renewal.Cursor.Drain.Rejected != 1 || receipt.Renewal.IntakeEnabled {
				t.Fatal(receipt)
			}
		})
	}
}
