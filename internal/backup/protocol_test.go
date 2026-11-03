//go:build linux

package backup

import (
	"bytes"
	"io"
	"testing"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupProtocolBindsAttemptAndCompletes(t *testing.T) {
	_, _, attempt, header, data := archiveFixture(t)
	var wire bytes.Buffer
	if err := protocol.WriteSnapshotTransfer(&wire, header, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	var received bytes.Buffer
	h, err := protocol.ReadSnapshotTransfer(bytes.NewReader(wire.Bytes()), &received, attempt, 1<<20)
	if err != nil || h.SHA256 != header.SHA256 || !bytes.Equal(data, received.Bytes()) {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]byte) []byte{"truncate": func(w []byte) []byte { return w[:len(w)-1] }, "trailing": func(w []byte) []byte { return append(w, 'x') }, "magic": func(w []byte) []byte { w[0] = 'x'; return w }, "body": func(w []byte) []byte { w[len(w)-200] ^= 1; return w }} {
		t.Run(name, func(t *testing.T) {
			copy := append([]byte{}, wire.Bytes()...)
			if _, err := protocol.ReadSnapshotTransfer(bytes.NewReader(mutate(copy)), io.Discard, attempt, 1<<20); err == nil {
				t.Fatal("bad stream accepted")
			}
		})
	}
	attempt.Nonce = testutil.ID(t, "n")
	if _, err = protocol.ReadSnapshotTransfer(bytes.NewReader(wire.Bytes()), io.Discard, attempt, 1<<20); err == nil {
		t.Fatal("replayed snapshot accepted")
	}
}

func FuzzBackupFrameBounds(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{255, 255, 255, 255})
	f.Add([]byte{0, 0, 0, 2, '{', '}'})
	f.Fuzz(func(t *testing.T, data []byte) {
		body, err := protocol.ReadBackupFrame(bytes.NewReader(data))
		if err == nil && len(body) > protocol.MaxBackupHeader {
			t.Fatal("frame exceeded bound")
		}
		_, _ = io.Copy(io.Discard, bytes.NewReader(body))
	})
}
