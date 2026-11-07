package signalaccount

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func nativeLinkFixture(t *testing.T) NativeCLI {
	t.Helper()
	dir := t.TempDir()
	source := `package main
import("fmt";"os";"path/filepath";"syscall")
func main(){
 if syscall.Flock(3,syscall.LOCK_EX|syscall.LOCK_NB)!=nil { os.Exit(8) }
 if len(os.Args)==2 && os.Args[1]=="--version" { fmt.Println("signal-cli 0.14.9");return }
 if len(os.Args)!=6 || os.Args[1]!="--config" || os.Args[3]!="link" || os.Args[4]!="--name" { os.Exit(9) }
 f,e:=os.OpenFile(filepath.Join(os.Getenv("HOME"),"fixture-count"),os.O_CREATE|os.O_APPEND|os.O_WRONLY,0600);if e!=nil{os.Exit(10)};f.Write([]byte("1"));f.Close()
 fmt.Println("private-fixture-link-uri")
 if _,e:=os.Stat(filepath.Join(os.Getenv("HOME"),"fail"));e==nil{os.Exit(1)}
}
`
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(dir, "signal-cli")
	cmd := exec.Command("go", "build", "-o", cli, file)
	if out, err := process.LimitedOutput(cmd, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	if err := os.Chmod(cli, 0700); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(cli)
	if err != nil {
		t.Fatal(err)
	}
	return NativeCLI{Path: cli, SHA256: fmt.Sprintf("%x", sha256.Sum256(body)), Version: "0.14.9"}
}

func TestNativeLinkPinnedExecutableAndUncertainRetry(t *testing.T) {
	native := nativeLinkFixture(t)
	for _, fail := range []bool{false, true} {
		path := t.TempDir()
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
		if fail {
			if err := os.WriteFile(filepath.Join(path, "fail"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		id, err := safefs.NewID("n")
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		status, err := LinkNative(context.Background(), path, "fixture-device", id, native, &out)
		if (err != nil) != fail || !strings.Contains(out.String(), "private-fixture-link-uri") || status.RuntimeAllowed == fail {
			t.Fatal(status, err, out.String())
		}
		out.Reset()
		_, err = LinkNative(context.Background(), path, "fixture-device", id, native, &out)
		if (err != nil) != fail || out.Len() != 0 {
			t.Fatal("retry launched or changed link result", err)
		}
		count, err := os.ReadFile(filepath.Join(path, "fixture-count"))
		if err != nil || string(count) != "1" {
			t.Fatal("link replayed", err, string(count))
		}
		root, err := safefs.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		history, err := readHistory(root)
		root.Close()
		if err != nil || len(history.Records) != 1 || history.Records[0].Intent.Native == nil || *history.Records[0].Intent.Native != native {
			t.Fatal("executable pin lost", err)
		}
		records, _ := json.Marshal(history)
		if bytes.Contains(records, []byte("private-fixture-link-uri")) {
			t.Fatal("link URI entered retained evidence")
		}
		changed := native
		changed.Path = filepath.Join(filepath.Dir(native.Path), "other-native")
		body, err := os.ReadFile(native.Path)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(changed.Path, body, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err = LinkNative(context.Background(), path, "fixture-device", id, changed, io.Discard); err == nil {
			t.Fatal("changed executable reused operation")
		}
		if _, err = Link(context.Background(), path, "fixture-device", id, io.Discard); err == nil {
			t.Fatal("PATH linker reused native operation")
		}
	}
}

func TestNativeLinkPreflightDoesNotCreateAttempt(t *testing.T) {
	native := nativeLinkFixture(t)
	for _, scenario := range []string{"digest", "script", "busy", "version"} {
		t.Run(scenario, func(t *testing.T) {
			path, id, _, _ := fixture(t)
			n := native
			switch scenario {
			case "digest":
				n.SHA256 = strings.Repeat("a", 64)
			case "script":
				n.Path = filepath.Join(t.TempDir(), "script")
				body := []byte("#!/bin/sh\necho must-not-run\n")
				if err := os.WriteFile(n.Path, body, 0700); err != nil {
					t.Fatal(err)
				}
				n.SHA256 = fmt.Sprintf("%x", sha256.Sum256(body))
			case "busy":
				lease, err := AcquireRuntime(path)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Close()
			case "version":
				n.Version = "unsupported"
			}
			if _, err := LinkNative(context.Background(), path, "device", id, n, io.Discard); err == nil {
				t.Fatal("invalid link admitted")
			}
			if _, err := os.Stat(filepath.Join(path, headFile)); !os.IsNotExist(err) {
				t.Fatal("preflight wrote a link attempt", err)
			}
		})
	}
}
