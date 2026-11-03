package signalaccount

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type linker struct {
	now   func() time.Time
	run   func(context.Context, *Lease, string, io.Writer) error
	fault func(string) error
}

func (l linker) boundary(point string) error {
	if l.fault != nil {
		return l.fault(point)
	}
	return nil
}

// Link is the in-container/native entry point, not a service coordinator. Its
// caller must stop the relay first. A busy account fails before any journal or
// CLI invocation. Repeating an uncertain attempt never executes link again.
func Link(ctx context.Context, path, name, operation string, out io.Writer) (Status, error) {
	return (linker{now: time.Now, run: runLink}).link(ctx, path, name, operation, out)
}

func (l linker) link(ctx context.Context, path, name, operation string, out io.Writer) (status Status, retErr error) {
	if !protocol.ValidToken(name) || !safefs.ValidID(operation, "n") || out == nil {
		return Status{}, fmt.Errorf("invalid Signal device name or output")
	}
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	d, guard, err := control(path, false)
	if err != nil {
		return Status{}, err
	}
	defer d.Close()
	defer guard.Close()
	if err = requireNoRecovery(d); err != nil {
		return Status{}, err
	}
	defer func() {
		if e := applyMaintenanceStatus(d, &status); e != nil && retErr == nil {
			retErr = e
		}
	}()
	if err = linkMaintenanceAllowed(d, operation, name); err != nil {
		return Status{}, err
	}
	lease, err := acquire(path)
	if err != nil {
		return Status{}, err
	}
	defer lease.Close()
	h, err := readHistory(lease.dir)
	if err != nil {
		return Status{}, err
	}
	var r linkRecord
	for i, old := range h.Records {
		if old.Intent.ID == operation && i != len(h.Records)-1 {
			return h.status(), fmt.Errorf("Signal link operation is historical; inspect its retained record")
		}
	}
	if len(h.Records) > 0 {
		last := h.Records[len(h.Records)-1]
		if last.Intent.ID == operation {
			if last.Intent.Name != name {
				return h.status(), fmt.Errorf("Signal link operation reused with another device name")
			}
			if last.complete() {
				return h.status(), nil
			}
			if last.Start != nil {
				return h.status(), fmt.Errorf("Signal link %s is uncertain; inspect account state and explicitly resolve it before another attempt", last.Intent.ID)
			}
			r = last
		} else if !last.complete() {
			return h.status(), fmt.Errorf("previous Signal link operation is unfinished")
		}
	}
	if r.Intent.ID == "" {
		if len(h.Records) >= maxLinks {
			return h.status(), fmt.Errorf("Signal link history is full")
		}
		if err = lease.dir.Capacity(64 << 10); err != nil {
			return h.status(), err
		}
		r.Intent = linkIntent{Version: 1, Sequence: len(h.Records) + 1, ID: operation, Name: name, CreatedAt: l.now().UTC()}
		if len(h.Records) > 0 {
			last := h.Records[len(h.Records)-1]
			r.Intent.Previous = protocol.Hash(last)
			if r.Intent.CreatedAt.Before(h.status().FinishedAt) {
				return h.status(), fmt.Errorf("Signal link clock moved backward")
			}
		}
		if err = lease.dir.Mkdir(historyDirectory); err != nil {
			return h.status(), err
		}
		if err = writeRecord(lease.dir, historyDirectory+"/"+recordFile(r.Intent.Sequence, "intent"), r.Intent, false); err != nil {
			return h.status(), err
		}
		if err = l.boundary("link_intent"); err != nil {
			return Status{}, err
		}
	}
	if h.Head.Sequence != r.Intent.Sequence {
		head := linkHead{1, r.Intent.Sequence, r.Intent.ID, protocol.Hash(r.Intent)}
		if err = writeRecord(lease.dir, headFile, head, h.Head.Sequence != 0); err != nil {
			return Status{}, err
		}
	}
	if err = l.boundary("link_head"); err != nil {
		return Status{}, err
	}
	if err = ctx.Err(); err != nil {
		return Status{}, err
	}
	start := linkStart{protocol.Hash(r.Intent), l.now().UTC()}
	if start.At.Before(r.Intent.CreatedAt) {
		return Status{}, fmt.Errorf("Signal link clock moved backward")
	}
	if err = writeRecord(lease.dir, historyDirectory+"/"+recordFile(r.Intent.Sequence, "start"), start, false); err != nil {
		return Status{}, err
	}
	if err = l.boundary("link_started"); err != nil {
		return Status{}, err
	}
	guard.Close() // The durable launch marker now fences new maintenance.
	cliErr := l.run(ctx, lease, name, out)
	if err = l.boundary("link_child_finished"); err != nil {
		return Status{}, err
	}
	result := linkResult{Intent: protocol.Hash(r.Intent), Start: protocol.Hash(start), Outcome: "uncertain", At: l.now().UTC()}
	if cliErr == nil {
		result.Outcome = "linked"
	}
	if result.At.Before(start.At) {
		return Status{}, fmt.Errorf("Signal link clock moved backward; account remains fenced")
	}
	if err = writeRecord(lease.dir, historyDirectory+"/"+recordFile(r.Intent.Sequence, "result"), result, false); err != nil {
		return Status{}, err
	}
	if err = l.boundary("link_result"); err != nil {
		return Status{}, err
	}
	h, err = readHistory(lease.dir)
	if err != nil {
		return Status{}, err
	}
	if cliErr != nil {
		return h.status(), fmt.Errorf("Signal link %s was not confirmed; account remains fenced for explicit review", r.Intent.ID)
	}
	return h.status(), nil
}

type linkOutput struct {
	writer    io.Writer
	remaining int
}

func (w *linkOutput) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, fmt.Errorf("Signal link output exceeds budget")
	}
	n, err := w.writer.Write(p)
	w.remaining -= n
	return n, err
}

func runLink(ctx context.Context, l *Lease, name string, out io.Writer) error {
	path := l.dir.File().Name()
	cmd := exec.Command("signal-cli", "--config", path, "link", "--name", name)
	cmd.ExtraFiles = []*os.File{l.File()}
	cmd.Env = []string{"PATH=/opt/signal-cli/bin:/opt/java/openjdk/bin:/usr/local/bin:/usr/bin:/bin", "HOME=" + path, "LANG=C.UTF-8", "JAVA_TOOL_OPTIONS=-Xmx256m"}
	// Linking URI is streamed only to the explicitly invoked command's stdout.
	// It is never copied into the account journal or returned in errors.
	cmd.Stdout = &linkOutput{writer: out, remaining: 32 << 10}
	cmd.Stderr = io.Discard
	return process.RunContext(ctx, cmd, 5*time.Minute, 5*time.Second)
}

// Resolve acknowledges an operator's inspection of possibly changed account
// state. It does not claim linking succeeded, verify an account or execute a CLI.
// The inherited account lease must be free, proving no cooperating child remains.
func Resolve(path, operation, reason string) (status Status, retErr error) {
	if err := ValidateResolution(operation, reason); err != nil {
		return Status{}, err
	}
	d, guard, err := control(path, false)
	if err != nil {
		return Status{}, err
	}
	defer d.Close()
	defer guard.Close()
	if err = requireNoRecovery(d); err != nil {
		return Status{}, err
	}
	defer func() {
		if e := applyMaintenanceStatus(d, &status); e != nil && retErr == nil {
			retErr = e
		}
	}()
	l, err := acquire(path)
	if err != nil {
		return Status{}, err
	}
	defer l.Close()
	h, err := readHistory(l.dir)
	if err != nil {
		return Status{}, err
	}
	if len(h.Records) == 0 || h.Records[len(h.Records)-1].Intent.ID != operation {
		return h.status(), fmt.Errorf("Signal link operation changed")
	}
	r := h.Records[len(h.Records)-1]
	if r.Resolution != nil {
		if r.Resolution.Reason != reason {
			return h.status(), fmt.Errorf("Signal link review already recorded with another reason")
		}
		return h.status(), nil
	}
	if r.complete() {
		return h.status(), fmt.Errorf("Signal link is already confirmed")
	}
	if h.Pending {
		head := linkHead{1, r.Intent.Sequence, r.Intent.ID, protocol.Hash(r.Intent)}
		if err = writeRecord(l.dir, headFile, head, h.Head.Sequence != 0); err != nil {
			return h.status(), err
		}
	}
	v := linkResolution{Intent: protocol.Hash(r.Intent), Start: protocol.Hash(r.Start), Result: protocol.Hash(r.Result), Reason: reason, At: time.Now().UTC()}
	last := r.Intent.CreatedAt
	if r.Start != nil {
		last = r.Start.At
	}
	if r.Result != nil {
		last = r.Result.At
	}
	if v.At.Before(last) {
		return h.status(), fmt.Errorf("Signal link clock moved backward")
	}
	if err = writeRecord(l.dir, historyDirectory+"/"+recordFile(r.Intent.Sequence, "resolution"), v, false); err != nil {
		return h.status(), err
	}
	h, err = readHistory(l.dir)
	if err != nil {
		return Status{}, err
	}
	return h.status(), nil
}

func ValidateResolution(operation, reason string) error {
	if !safefs.ValidID(operation, "n") || !validReason(reason) {
		return fmt.Errorf("resolution requires exact operation ID and a bounded single-line reason")
	}
	return nil
}
