package relay

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/testutil"
)

// The CLI executable only emulates linking/version output. All delivery calls
// terminate in the injected command callback; no Signal provider is contacted.
func TestFreshManagedRelayDirectAlertsWithCanonicalInbox(t *testing.T) {
	bin := t.TempDir()
	source := filepath.Join(bin, "main.go")
	testutil.Must(t, os.WriteFile(source, []byte(`package main
import("fmt";"os")
func main(){if len(os.Args)==2 && os.Args[1]=="--version" {fmt.Println("signal-cli 0.14.9");return};fmt.Println("synthetic-link-uri-no-network")}
`), 0600))
	cli := filepath.Join(bin, "signal-cli")
	if out, err := exec.Command("go", "build", "-o", cli, source).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	testutil.Must(t, os.Chmod(cli, 0700))
	raw, err := os.ReadFile(cli)
	testutil.Must(t, err)
	native := signalaccount.NativeCLI{Path: cli, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), Version: "0.14.9"}
	accountPath := freshInboxTestParent(t)
	status, err := signalaccount.LinkNative(context.Background(), accountPath, "fixture", testutil.ID(t, "n"), native, io.Discard)
	testutil.Must(t, err)
	binding := signalaccount.NativeServiceBinding{Version: 1, ServiceRoot: filepath.Join(t.TempDir(), "service"), ServiceHash: strings.Repeat("b", 64), Account: fixtureAccount, LinkOperation: status.Operation, LinkHash: status.RecordHash, CLISHA256: native.SHA256, CLIVersion: native.Version}
	testutil.Must(t, signalaccount.BindNativeService(accountPath, binding))
	var admitted atomic.Bool
	admitted.Store(true)
	check := func() error {
		if !admitted.Load() {
			return fmt.Errorf("synthetic admission withdrawn")
		}
		return nil
	}
	account, err := signalaccount.OpenNativeRuntime(accountPath, binding, check)
	testutil.Must(t, err)
	defer account.Close()
	b, err := BootstrapFreshInbox(context.Background(), filepath.Join(freshInboxTestParent(t), "inbox"), testutil.ID(t, "n"), fixtureAccount)
	testutil.Must(t, err)
	inbox, err := OpenFreshSignalInbox(context.Background(), b, func(context.Context) error { return check() })
	testutil.Must(t, err)
	defer inbox.Close()
	short, err := os.MkdirTemp("", "fresh-relay-")
	testutil.Must(t, err)
	defer os.RemoveAll(short)
	selector, recipient := filepath.Join(short, "account"), filepath.Join(short, "recipient")
	testutil.Must(t, os.WriteFile(selector, []byte("+10000000000"), 0600))
	testutil.Must(t, os.WriteFile(recipient, []byte(fixtureActor), 0600))
	c := Config{StateDir: accountPath, Socket: filepath.Join(short, "api.sock"), AccountFile: selector, RecipientFile: recipient}
	var sends atomic.Int32
	execution := FreshExecution{Account: account, Inbox: inbox, Check: func(context.Context) error { return check() }, Health: func(context.Context) (any, error) { return map[string]bool{"synthetic": true}, check() }, Command: func(_ context.Context, lease *os.File, out io.Writer, args ...string) error {
		if lease == nil {
			return fmt.Errorf("command lost account lease")
		}
		joined := strings.Join(args, " ")
		switch {
		case strings.HasSuffix(joined, "--version"):
			_, err := fmt.Fprintln(out, "signal-cli 0.14.9")
			return err
		case strings.HasSuffix(joined, "listAccounts"):
			_, err := fmt.Fprintf(out, `[{"number":"+10000000000","aci":%q}]`, fixtureAccount)
			return err
		case strings.Contains(joined, " send "):
			sends.Add(1)
			return nil
		}
		return fmt.Errorf("unexpected synthetic command")
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunFreshManaged(ctx, c, execution) }()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, e := client.Get("http://localhost/health")
		if e == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("fresh relay did not start", e)
		}
		select {
		case err := <-done:
			t.Fatal("fresh relay exited", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, destination := range []string{Destination("+10000000000", fixtureActor), strings.Repeat("0", 64)} {
		body := fmt.Sprintf(`{"id":"fixture","message":"synthetic message","destination":%q}`, destination)
		response, e := client.Post("http://localhost/send", "application/json", strings.NewReader(body))
		testutil.Must(t, e)
		response.Body.Close()
		expected := http.StatusOK
		if strings.HasPrefix(destination, "000") {
			expected = http.StatusConflict
		}
		if response.StatusCode != expected {
			t.Fatal("wrong direct destination result", response.StatusCode)
		}
	}
	if sends.Load() != 1 {
		t.Fatal("unmatched destination executed CLI")
	}
	admitted.Store(false)
	if response, e := client.Get("http://localhost/health"); e == nil {
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("lost admission stayed healthy")
		}
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("lost admission reported success")
		}
	case <-ctx.Done():
		t.Fatal("relay ignored withdrawn admission")
	}
	if inbox.Cursor().Position != 0 {
		t.Fatal("send-only relay fabricated command intake")
	}
}
