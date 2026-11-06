package install

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type commandFixtureTransport struct {
	polled  atomic.Bool
	replies atomic.Int32
}

func (t *commandFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := ""
	switch {
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		body = `{"ok":true,"result":[]}`
		if t.polled.CompareAndSwap(false, true) {
			body = fmt.Sprintf(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":10,"date":%d,"text":"worker/local status","from":{"id":789},"chat":{"id":456}}}]}`, time.Now().Unix())
		}
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		t.replies.Add(1)
		body = `{"ok":true,"result":{"message_id":777,"from":{"id":123,"is_bot":true},"chat":{"id":456}}}`
	default:
		return (telegramCheckTransport{}).RoundTrip(r)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func TestManagedCommandsProviderIntakeConversionAndReply(t *testing.T) {
	spec, i, c, active, record := managedCommandsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	m, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openManagedStore(m, c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	lease, err := d.Lock("installation.lock", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	check := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.RequireManagedCommandHistory(m.CommandHistoryHash); err != nil {
			return err
		}
		if _, err := readManagedCommandAdmission(d, m, c); err != nil {
			return err
		}
		return checkDeliveryAdmission(d, m, c, time.Now().UTC())
	}
	if err = s.BindManagedCommandAuthority(check); err != nil {
		t.Fatal(err)
	}
	transport := &commandFixtureTransport{}
	failed := make(chan error, 1)
	services, err := managedCommandServices(d, s, c, m, check, func(e error) {
		select {
		case failed <- e:
		default:
		}
	}, transport)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runRuntimeGroup(ctx, services) }()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for transport.replies.Load() == 0 {
		select {
		case err := <-failed:
			cancel()
			<-done
			t.Fatal(err)
		case <-ctx.Done():
			<-done
			t.Fatal("no durable command reply", ctx.Err())
		case <-ticker.C:
		}
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	commands, err := s.Commands(context.Background(), c.Worker.Producers[0].Producer)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].Command.Actor != "789" || commands[0].Command.Destination.NativeID != "456" || commands[0].Command.Descriptor.Effect != "read" {
		t.Fatal("provider conversion lost identity or scope", commands)
	}
	if transport.replies.Load() != 1 {
		t.Fatal("reply duplicated", transport.replies.Load())
	}
}

func TestManagedCommandsTransportPinsActualCredential(t *testing.T) {
	for _, kind := range []string{"qualified", "changed-file", "changed-request", "lost-admission"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(path, []byte("123:qualified\n"), 0600); err != nil {
				t.Fatal(err)
			}
			failed := false
			base := &commandFixtureTransport{}
			check := func(context.Context) error { return nil }
			if kind == "lost-admission" {
				check = func(context.Context) error { return fmt.Errorf("admission lost") }
			}
			tr := admittedTelegramTransport{check: check, fail: func(error) { failed = true }, tokenFile: path, tokenHash: managedDigest([]byte("123:qualified\n")), next: base}
			token := "123:qualified"
			if kind == "changed-file" {
				if err := os.WriteFile(path, []byte("123:other"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "changed-request" {
				token = "123:other"
			}
			req, err := http.NewRequest(http.MethodPost, "https://api.telegram.org/bot"+token+"/getUpdates", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := tr.RoundTrip(req)
			if resp != nil {
				resp.Body.Close()
			}
			if kind == "qualified" {
				if err != nil || failed || !base.polled.Load() {
					t.Fatal(err, failed)
				}
			} else if err == nil || !failed || base.polled.Load() {
				t.Fatal("unqualified request reached provider", err, failed)
			}
		})
	}
}
