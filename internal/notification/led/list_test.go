package led

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
)

type registryPage struct {
	States []Entry `json:"states"`
	Total  int     `json:"total"`
	Next   *int    `json:"next_offset"`
}

func registryClient(t *testing.T, pages []registryPage, delay time.Duration) Client {
	t.Helper()
	var pending sync.WaitGroup
	t.Cleanup(pending.Wait)
	index, offset := 0, 0
	return Client{Budget: time.Second, dial: func(ctx context.Context) (net.Conn, error) {
		client, conn := net.Pipe()
		if index >= len(pages) {
			t.Error("unexpected extra listing request")
			conn.Close()
			return client, nil
		}
		page, expectedOffset := pages[index], offset
		index++
		if page.Next != nil {
			offset = *page.Next
		}
		pending.Add(1)
		go func() {
			defer pending.Done()
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			var request request
			err := json.NewDecoder(conn).Decode(&request)
			if err != nil || request.Operation != "state.list" || request.Offset == nil || *request.Offset != expectedOffset {
				t.Error("unexpected listing request", err)
				conn.Close()
				return
			}
			if delay > 0 {
				time.Sleep(delay)
			}
			body, _ := json.Marshal(page)
			_ = json.NewEncoder(conn).Encode(reply{Version: 1, OK: true, Result: body})
		}()
		return client, nil
	}}
}

func TestRegistryListingRejectsIncompleteAndChangingPages(t *testing.T) {
	a := Entry{State: State{ID: "a", LED: "green", Pattern: "off"}, OwnerUID: 1000}
	b := a
	b.ID = "b"
	next, skip := 1, 2
	for _, tc := range []struct {
		name  string
		pages []registryPage
		valid bool
	}{
		{"complete", []registryPage{{States: []Entry{a}, Total: 2, Next: &next}, {States: []Entry{b}, Total: 2}}, true},
		{"empty", []registryPage{{Total: 0}}, true},
		{"missing-final", []registryPage{{States: []Entry{a}, Total: 2}}, false},
		{"duplicate", []registryPage{{States: []Entry{a}, Total: 2, Next: &next}, {States: []Entry{a}, Total: 2}}, false},
		{"out-of-order", []registryPage{{States: []Entry{b, a}, Total: 2}}, false},
		{"skipped-offset", []registryPage{{States: []Entry{a}, Total: 3, Next: &skip}}, false},
		{"empty-nonfinal", []registryPage{{Total: 2, Next: &next}}, false},
		{"invalid-entry", []registryPage{{States: []Entry{{State: State{ID: "invalid!", LED: "green", Pattern: "off"}}}, Total: 1}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := registryClient(t, tc.pages, 0)
			_, err := c.List(context.Background())
			if (err == nil) != tc.valid {
				t.Fatal("unexpected listing result", err)
			}
		})
	}
}

func TestRegistryListingSharesOneDeadlineAcrossPages(t *testing.T) {
	next := 1
	a := Entry{State: State{ID: "a", LED: "green", Pattern: "off"}, OwnerUID: 1000}
	b := a
	b.ID = "b"
	c := registryClient(t, []registryPage{{States: []Entry{a}, Total: 2, Next: &next}, {States: []Entry{b}, Total: 2}}, 75*time.Millisecond)
	c.Budget = 120 * time.Millisecond
	if _, err := c.List(context.Background()); err == nil {
		t.Fatal("each page received a fresh latency budget")
	}
}
