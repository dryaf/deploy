package workerctl

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

func TestControlStreamKeepsLinkOutputSeparate(t *testing.T) {
	const secret = "synthetic-private-link-uri"
	var wire bytes.Buffer
	writer := linkEventWriter{out: &wire}
	if _, err := writer.Write([]byte(secret)); err != nil {
		t.Fatal(err)
	}
	result := ControlResult{Version: 1, State: "configured", Code: "ok", Next: "check qualification"}
	if err := writeControlFrame(&wire, controlEvent{Version: 1, Kind: "result", Result: &result}); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []int{1, 3, 4, 7, 1024} {
		var display bytes.Buffer
		receiver := controlEventReceiver{display: &display}
		raw := wire.Bytes()
		for len(raw) > 0 {
			n := min(chunk, len(raw))
			if _, err := receiver.Write(raw[:n]); err != nil {
				t.Fatal(err)
			}
			raw = raw[n:]
		}
		actual, err := receiver.finish()
		if err != nil || actual.State != result.State || display.String() != secret {
			t.Fatal("stream lost result or private output", err)
		}
		if strings.Contains(actual.Next, secret) {
			t.Fatal("link secret appeared in final progress")
		}
	}
	receiver := controlEventReceiver{}
	if _, err := receiver.Write(wire.Bytes()); err == nil {
		t.Fatal("link payload accepted without private display")
	}
}
func TestControlStreamRejectsUnboundedAndUncertainResponses(t *testing.T) {
	for _, event := range []controlEvent{{Version: 2, Kind: "link", Link: []byte("private")}, {Version: 1, Kind: "unknown"}, {Version: 1, Kind: "link", Link: bytes.Repeat([]byte("x"), 4097)}, {Version: 1, Kind: "result", Result: &ControlResult{Version: 1}, Link: []byte("secret")}} {
		var wire bytes.Buffer
		if err := writeControlFrame(&wire, event); err != nil {
			t.Fatal(err)
		}
		var display bytes.Buffer
		receiver := controlEventReceiver{display: &display}
		if _, err := receiver.Write(wire.Bytes()); err == nil || display.Len() != 0 {
			t.Fatal("invalid event reached private display")
		}
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], maxControlFrame+1)
	receiver := controlEventReceiver{display: io.Discard}
	if _, err := receiver.Write(header[:]); err == nil {
		t.Fatal("oversized frame accepted")
	}
	receiver = controlEventReceiver{display: io.Discard}
	if _, err := receiver.finish(); err == nil {
		t.Fatal("lost completion became success")
	}
	var wire bytes.Buffer
	if err := writeControlFrame(&wire, controlEvent{Version: 1, Kind: "result", Result: &ControlResult{Version: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Write(wire.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Write([]byte("trailing")); err == nil {
		t.Fatal("trailing output accepted")
	}
	writer := linkEventWriter{out: io.Discard}
	if _, err := writer.Write(make([]byte, maxLinkOutput+1)); err == nil {
		t.Fatal("unbounded linking output accepted")
	}
}
