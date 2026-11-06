package relay

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestRelaySocketRetainsOnlyItsOwnPath(t *testing.T) {
	root, err := os.MkdirTemp("", "relay-socket-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	path := filepath.Join(root, "api.sock")
	l, check, cleanup, err := openRelaySocket(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = check(); err != nil {
		t.Fatal(err)
	}
	if other, _, release, err := openRelaySocket(path); err == nil {
		other.Close()
		release()
		t.Fatal("live socket replaced")
	}
	if err = os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err = check(); err == nil {
		t.Fatal("changed socket accepted")
	}
	l.Close()
	cleanup()
	if _, err = os.Lstat(path); err != nil {
		t.Fatal("cleanup removed replacement", err)
	}
}
