package workerctl

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/dryaf/deploy/internal/safefs"
)

// Linking output is a separate ephemeral channel. It never appears in the
// ordinary ControlResult, operation receipts or JSON progress output.
type controlEvent struct {
	Version int            `json:"version"`
	Kind    string         `json:"kind"`
	Link    []byte         `json:"link,omitempty"`
	Result  *ControlResult `json:"result,omitempty"`
}

const maxLinkOutput = 32 << 10

type linkEventWriter struct {
	out             io.Writer
	written, events int
}

func (w *linkEventWriter) Write(raw []byte) (int, error) {
	if len(raw) > maxLinkOutput-w.written {
		return 0, fmt.Errorf("Signal linking output exceeds limit")
	}
	sent := 0
	for len(raw) > 0 {
		n := min(len(raw), 4096)
		if w.events == 256 {
			return sent, fmt.Errorf("too many Signal linking events")
		}
		if err := writeControlFrame(w.out, controlEvent{Version: 1, Kind: "link", Link: raw[:n]}); err != nil {
			return sent, fmt.Errorf("private linking display disconnected")
		}
		w.events++
		w.written += n
		sent += n
		raw = raw[n:]
	}
	return sent, nil
}

// controlEventReceiver incrementally parses SSH stdout without buffering linking
// payloads in ordinary command logs. The caller supplies a private display.
type controlEventReceiver struct {
	display           io.Writer
	buffer            []byte
	result            *ControlResult
	linkBytes, events int
	failure           error
}

func (r *controlEventReceiver) Write(raw []byte) (int, error) {
	if r.failure != nil {
		return 0, r.failure
	}
	total := len(raw)
	fail := func(message string) (int, error) {
		r.buffer = nil
		r.failure = fmt.Errorf("%s", message)
		return 0, r.failure
	}
	for len(raw) > 0 {
		if r.result != nil {
			return fail("unexpected control data after final result")
		}
		needed := 4 - len(r.buffer)
		if len(r.buffer) >= 4 {
			size := int(binary.BigEndian.Uint32(r.buffer[:4]))
			if size == 0 || size > maxControlFrame {
				return fail("invalid streaming control frame size")
			}
			needed = size + 4 - len(r.buffer)
		}
		n := min(needed, len(raw))
		r.buffer = append(r.buffer, raw[:n]...)
		raw = raw[n:]
		if len(r.buffer) < 4 {
			continue
		}
		size := int(binary.BigEndian.Uint32(r.buffer[:4]))
		if size == 0 || size > maxControlFrame {
			return fail("invalid streaming control frame size")
		}
		if len(r.buffer) != size+4 {
			continue
		}
		var event controlEvent
		if safefs.DecodeJSON(r.buffer[4:], &event, maxControlFrame) != nil || event.Version != 1 {
			return fail("invalid streaming control event")
		}
		r.buffer = nil
		r.events++
		if r.events > 257 {
			return fail("too many streaming control events")
		}
		switch event.Kind {
		case "link":
			if event.Result != nil || len(event.Link) == 0 || len(event.Link) > 4096 || r.linkBytes+len(event.Link) > maxLinkOutput || r.display == nil {
				return fail("unexpected or oversized private linking output")
			}
			if n, err := r.display.Write(event.Link); err != nil || n != len(event.Link) {
				return fail("private linking display disconnected")
			}
			r.linkBytes += len(event.Link)
		case "result":
			if event.Result == nil || event.Result.Version != 1 || len(event.Link) != 0 {
				return fail("invalid final control result")
			}
			r.result = event.Result
		default:
			return fail("unknown streaming control event")
		}
	}
	return total, nil
}
func (r *controlEventReceiver) finish() (ControlResult, error) {
	if r.failure != nil {
		return ControlResult{}, r.failure
	}
	if r.result == nil || len(r.buffer) != 0 {
		return ControlResult{}, fmt.Errorf("streaming control interrupted; inspect and resume the same declaration")
	}
	return *r.result, nil
}

// ControlMainStream uses the same stdin request and redacted completion as the
// ordinary helper, with a bounded ephemeral channel solely for device linking.
func ControlMainStream(ctx context.Context, in io.Reader, out io.Writer) error {
	var request ControlRequest
	result := ControlResult{Version: 1, State: "needs_action", Code: "invalid_request"}
	if err := readControlFrame(in, &request); err != nil {
		result.Next = err.Error()
	} else {
		var err error
		result, err = applyControlWithLinkOutput(ctx, request, &linkEventWriter{out: out})
		if err != nil {
			result = controlFailure(err)
		}
	}
	return writeControlFrame(out, controlEvent{Version: 1, Kind: "result", Result: &result})
}
