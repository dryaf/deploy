package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func verifySignalAccount(raw []byte, selector, expected string) error {
	var accounts []struct {
		Number string `json:"number"`
		ACI    string `json:"aci"`
	}
	if err := safefs.ReadJSON(bytes.NewReader(raw), &accounts, 32<<10); err != nil {
		return fmt.Errorf("invalid Signal account identity response")
	}
	matches := 0
	for _, account := range accounts {
		if account.Number == selector || account.ACI == selector {
			if account.ACI != expected {
				return fmt.Errorf("Signal account identity changed")
			}
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("Signal account identity unavailable or ambiguous")
	}
	return nil
}

func (c InteractiveConfig) Validate() error {
	if c.Version != "0.14.9" || !provider.ValidSignalAccount(c.Account) || !filepath.IsAbs(c.InboxDir) || len(c.Destinations) == 0 || len(c.Destinations) > 32 || len(c.Actors) > 1024 {
		return fmt.Errorf("invalid interactive relay enrollment")
	}
	ids := map[string]bool{}
	native := map[string]bool{}
	for _, d := range c.Destinations {
		if d.Provider != "signal" || d.Account != c.Account || !protocol.ValidToken(d.ID) || !protocol.ValidToken(d.Generation) || d.Thread != "" || ids[d.ID] || native[d.NativeID] {
			return fmt.Errorf("invalid Signal conversation")
		}
		if strings.HasPrefix(d.NativeID, "group:") {
			g := strings.TrimPrefix(d.NativeID, "group:")
			raw, err := base64.StdEncoding.DecodeString(g)
			if err != nil || len(raw) != 32 || base64.StdEncoding.EncodeToString(raw) != g {
				return fmt.Errorf("invalid Signal group")
			}
		} else if !provider.ValidSignalAccount(d.NativeID) {
			return fmt.Errorf("Signal direct conversations require immutable recipient UUIDs")
		}
		ids[d.ID] = true
		native[d.NativeID] = true
	}
	for _, actor := range c.Actors {
		if !provider.ValidSignalAccount(actor) || actor == c.Account {
			return fmt.Errorf("invalid Signal actor")
		}
	}
	return nil
}

type receiveWriter struct {
	ctx      context.Context
	inbox    *Inbox
	selector string
	config   InteractiveConfig
	buffer   []byte
	count    int
	err      error
	discard  hash.Hash
}

func (w *receiveWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	written := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			i = len(p)
		}
		if w.discard == nil && len(w.buffer)+i > 32<<10 {
			w.discard = sha256.New()
			_, _ = w.discard.Write(w.buffer)
			w.buffer = nil
		}
		if w.discard != nil {
			_, _ = w.discard.Write(p[:i])
		} else {
			w.buffer = append(w.buffer, p[:i]...)
		}
		p = p[i:]
		if len(p) == 0 {
			break
		}
		p = p[1:]
		if w.discard == nil && len(bytes.TrimSpace(w.buffer)) == 0 {
			w.buffer = nil
			continue
		}
		w.count++
		if w.count > 16 {
			w.err = fmt.Errorf("Signal receive batch too large")
			return 0, w.err
		}
		var event interaction.SpoolEvent
		var err error
		if w.discard != nil {
			digest := hex.EncodeToString(w.discard.Sum(nil))
			event = interaction.SpoolEvent{Key: "oversize-" + digest, Hash: digest}
			w.discard = nil
		} else {
			event, err = provider.NormalizeSignal(w.buffer, w.selector, w.config.Account, w.config.Destinations)
		}
		w.buffer = nil
		if err != nil {
			w.err = err
			return 0, err
		}
		if event.Inbound != nil && !slices.Contains(w.config.Actors, event.Inbound.Actor) {
			event.Inbound = nil
		}
		if err = w.inbox.Enqueue(w.ctx, event); err != nil {
			w.err = err
			return 0, err
		}
	}
	return written, nil
}

func installInteractiveHandlers(mux *http.ServeMux, inbox *Inbox, c InteractiveConfig, slots chan struct{}, run func(context.Context, io.Writer, ...string) error) {
	for _, op := range []string{"list", "ack"} {
		mux.HandleFunc("/inbound/"+op, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				http.Error(w, "method rejected", 405)
				return
			}
			var cursor interaction.SpoolCursor
			if err := safefs.ReadJSON(http.MaxBytesReader(w, r.Body, 4096), &cursor, 4096); err != nil {
				http.Error(w, "invalid cursor", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if op == "list" {
				page, err := inbox.List(r.Context(), cursor)
				if err != nil {
					http.Error(w, "relay cursor rejected", 409)
					return
				}
				_ = json.NewEncoder(w).Encode(page)
			} else {
				if err := inbox.Ack(r.Context(), cursor); err != nil {
					http.Error(w, "relay ACK rejected", 409)
					return
				}
				_ = json.NewEncoder(w).Encode(struct {
					OK bool `json:"ok"`
				}{true})
			}
		})
	}
	mux.HandleFunc("/reply", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method rejected", 405)
			return
		}
		var req struct {
			Identity    protocol.Identity `json:"identity"`
			Account     string            `json:"account"`
			Destination string            `json:"destination"`
			Fingerprint string            `json:"fingerprint"`
			Text        string            `json:"text"`
		}
		if safefs.ReadJSON(http.MaxBytesReader(w, r.Body, 64<<10), &req, 64<<10) != nil || !provider.ValidConversationText(req.Text) || req.Account != c.Account {
			http.Error(w, "invalid reply", 400)
			return
		}
		if req.Identity != inbox.Cursor().Identity {
			http.Error(w, "relay identity changed", 409)
			return
		}
		var destination *interaction.Destination
		for i := range c.Destinations {
			d := &c.Destinations[i]
			if d.ID == req.Destination && d.Fingerprint() == req.Fingerprint {
				destination = d
				break
			}
		}
		if destination == nil {
			http.Error(w, "conversation changed", 409)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "busy", 503)
			return
		}
		args := []string{"--output=json", "send", "-m", req.Text}
		if err := inbox.CheckActive(r.Context()); err != nil {
			http.Error(w, "relay recovery fenced", 409)
			return
		}
		if strings.HasPrefix(destination.NativeID, "group:") {
			args = append(args, "--group-id", strings.TrimPrefix(destination.NativeID, "group:"))
		} else {
			args = append(args, "--", destination.NativeID)
		}
		var output process.Buffer
		output.Limit = 16384
		if run(r.Context(), &output, args...) != nil {
			http.Error(w, "reply outcome uncertain", 502)
			return
		}
		var result struct {
			Timestamp int64 `json:"timestamp"`
		}
		raw := []byte(output.String())
		if !utf8.Valid(raw) || safefs.RejectDuplicateJSONKeys(raw) != nil || json.Unmarshal(raw, &result) != nil || result.Timestamp <= 0 {
			http.Error(w, "reply reference unavailable", 502)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Accepted bool   `json:"accepted"`
			NativeID string `json:"native_id"`
		}{true, strconv.FormatInt(result.Timestamp, 10)})
	})
}
