package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

const fixtureAccount = "11111111-1111-4111-8111-111111111111"
const fixtureActor = "22222222-2222-4222-8222-222222222222"

func TestSignalAccountIdentityPinnedBeforeReceiving(t *testing.T) {
	raw := []byte(fmt.Sprintf(`[{"number":"+10000000000","aci":%q}]`, fixtureAccount))
	if err := verifySignalAccount(raw, "+10000000000", fixtureAccount); err != nil {
		t.Fatal(err)
	}
	if err := verifySignalAccount(raw, fixtureAccount, fixtureAccount); err != nil {
		t.Fatal(err)
	}
	if verifySignalAccount(raw, "+10000000000", fixtureActor) == nil {
		t.Fatal("replacement account accepted")
	}
	if verifySignalAccount(raw, "+19999999999", fixtureAccount) == nil {
		t.Fatal("unknown account selector accepted")
	}
	if verifySignalAccount([]byte(`[]`), "+10000000000", fixtureAccount) == nil {
		t.Fatal("unlinked account accepted")
	}
}

func inboxFixture(t *testing.T) (*Inbox, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	inbox, err := OpenInbox(dir, fixtureAccount, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inbox.Close() })
	return inbox, dir
}

func TestInboxReplayAckAndRestoreFence(t *testing.T) {
	s, dir := inboxFixture(t)
	ctx := context.Background()
	cursor := s.Cursor()
	event := interaction.SpoolEvent{Key: "native-one", Hash: protocol.Hash("one")}
	if err := s.Enqueue(ctx, event); err != nil {
		t.Fatal(err)
	}
	page, err := s.List(ctx, cursor)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	r, err := OpenInbox(dir, fixtureAccount, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	replay, err := r.List(ctx, cursor)
	if err != nil || protocol.Hash(replay) != protocol.Hash(page) {
		t.Fatal("lost ACK did not replay", err)
	}
	if err = r.Enqueue(ctx, event); err != nil {
		t.Fatal(err)
	}
	event.Hash = protocol.Hash("changed")
	if r.Enqueue(ctx, event) == nil {
		t.Fatal("native event conflict accepted")
	}
	if err = r.Ack(ctx, page.Cursor); err != nil {
		t.Fatal(err)
	}
	if err = r.Ack(ctx, page.Cursor); err != nil {
		t.Fatal(err)
	}
	if _, err = r.List(ctx, cursor); err == nil {
		t.Fatal("restored gateway crossed acknowledged history")
	}
	bad := page.Cursor
	bad.Identity.Recovery = "wrong"
	if r.Ack(ctx, bad) == nil {
		t.Fatal("foreign relay epoch ACK")
	}
}

func TestSignalReceiveWriterCommitsCompleteLinesOnly(t *testing.T) {
	s, _ := inboxFixture(t)
	d := interaction.Destination{ID: "private", Generation: "one", Provider: "signal", Account: fixtureAccount, NativeID: fixtureActor}
	config := InteractiveConfig{Version: "0.14.9", Account: fixtureAccount, InboxDir: "/private", Destinations: []interaction.Destination{d}, Actors: []string{fixtureActor}}
	raw := fmt.Sprintf(`{"account":"+10000000000","envelope":{"sourceUuid":%q,"sourceDevice":1,"timestamp":1793443200000,"dataMessage":{"timestamp":1793443200000,"message":"backup/storage status"}}}`, fixtureActor)
	w := &receiveWriter{ctx: context.Background(), inbox: s, selector: "+10000000000", config: config}
	half := len(raw) / 2
	if _, err := w.Write([]byte(raw[:half])); err != nil {
		t.Fatal(err)
	}
	page, err := s.List(context.Background(), s.Cursor())
	if err != nil || len(page.Events) != 0 {
		t.Fatal("partial line committed", err)
	}
	if _, err = w.Write([]byte(raw[half:] + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("bad-json\n")); err == nil {
		t.Fatal("malformed receive accepted")
	}
	page, err = s.List(context.Background(), s.Cursor())
	if err != nil || len(page.Events) != 1 || page.Events[0].Inbound == nil {
		t.Fatal("committed prefix lost", err)
	}
}

func TestSignalReplyPinsDestinationAndUsesStructuredReference(t *testing.T) {
	s, _ := inboxFixture(t)
	d := interaction.Destination{ID: "private", Generation: "one", Provider: "signal", Account: fixtureAccount, NativeID: fixtureActor}
	c := InteractiveConfig{Version: "0.14.9", Account: fixtureAccount, InboxDir: "/private", Destinations: []interaction.Destination{d}}
	mux := http.NewServeMux()
	calls := 0
	installInteractiveHandlers(mux, s, c, make(chan struct{}, 1), func(_ context.Context, w io.Writer, args ...string) error {
		calls++
		if args[len(args)-1] != fixtureActor || args[len(args)-2] != "--" {
			t.Fatal("wrong recipient", args)
		}
		_, err := io.WriteString(w, `{"timestamp":1793443200000}`)
		return err
	})
	request := map[string]any{"identity": s.Cursor().Identity, "account": fixtureAccount, "destination": d.ID, "fingerprint": d.Fingerprint(), "text": "scope A only"}
	raw, _ := json.Marshal(request)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("POST", "/reply", strings.NewReader(string(raw))))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "1793443200000") {
		t.Fatal(response.Code, response.Body.String())
	}
	request["fingerprint"] = "changed"
	raw, _ = json.Marshal(request)
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("POST", "/reply", strings.NewReader(string(raw))))
	if response.Code != 409 || calls != 1 {
		t.Fatal("changed conversation sent")
	}
	request["fingerprint"] = d.Fingerprint()
	changed := s.Cursor().Identity
	changed.Recovery = "another-recovery"
	request["identity"] = changed
	raw, _ = json.Marshal(request)
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("POST", "/reply", strings.NewReader(string(raw))))
	if response.Code != 409 || calls != 1 {
		t.Fatal("reply accepted after relay identity changed")
	}
}

func TestOversizedSignalLineIsRejectedWithoutBlockingFollowingRecord(t *testing.T) {
	s, _ := inboxFixture(t)
	w := &receiveWriter{ctx: context.Background(), inbox: s, selector: "+10000000000", config: InteractiveConfig{Account: fixtureAccount}}
	for i := 0; i < 20; i++ {
		if _, err := w.Write([]byte(strings.Repeat("x", 4096))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write([]byte("\n{\"account\":\"+10000000000\",\"envelope\":{}}\n")); err != nil {
		t.Fatal(err)
	}
	page, err := s.List(context.Background(), s.Cursor())
	if err != nil || len(page.Events) != 2 {
		t.Fatal("oversized event blocked following input", err)
	}
	for _, e := range page.Events {
		if e.Inbound != nil {
			t.Fatal("unsupported input became command")
		}
	}
}
