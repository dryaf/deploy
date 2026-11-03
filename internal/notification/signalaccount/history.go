package signalaccount

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const maxLinks = 64
const historyDirectory = "deploy-link-history"
const headFile = "deploy-link-head.json"

type linkIntent struct {
	Version, Sequence  int
	ID, Name, Previous string
	CreatedAt          time.Time
}
type linkStart struct {
	Intent string
	At     time.Time
}
type linkResult struct {
	Intent, Start, Outcome string
	At                     time.Time
}
type linkResolution struct {
	Intent, Start, Result, Reason string
	At                            time.Time
}
type linkRecord struct {
	Intent     linkIntent
	Start      *linkStart
	Result     *linkResult
	Resolution *linkResolution
}
type linkHead struct {
	Version, Sequence int
	ID, Hash          string
}
type linkHistory struct {
	Head    linkHead
	Records []linkRecord
	Pending bool
}

func (r linkRecord) complete() bool {
	return (r.Result != nil && r.Result.Outcome == "linked") || r.Resolution != nil
}
func (h linkHistory) runtimeAllowed() bool {
	return !h.Pending && (len(h.Records) == 0 || h.Records[len(h.Records)-1].complete())
}
func recordFile(sequence int, kind string) string { return fmt.Sprintf("%04d.%s.json", sequence, kind) }
func validReason(reason string) bool {
	return reason == strings.TrimSpace(reason) && len(reason) > 0 && len(reason) <= 512 && utf8.ValidString(reason) && !strings.ContainsAny(reason, "\x00\r\n")
}

func readRecord(d *safefs.Directory, name string, v any) (bool, error) {
	raw, err := safefs.ReadFile(d, name, 4096)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var record struct {
		Body json.RawMessage
		Hash string
	}
	if err = safefs.DecodeJSON(raw, &record, 4096); err != nil {
		return true, err
	}
	if err = safefs.DecodeJSON(record.Body, v, 4096); err != nil {
		return true, err
	}
	if record.Hash != protocol.Hash(v) {
		return true, fmt.Errorf("Signal account evidence digest mismatch")
	}
	return true, nil
}

func writeRecord(d *safefs.Directory, name string, value any, replace bool) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(body) > 3072 {
		return fmt.Errorf("Signal account evidence exceeds budget")
	}
	return d.WriteJSON(name, struct {
		Body json.RawMessage
		Hash string
	}{body, protocol.Hash(value)}, replace)
}

func readHistory(d *safefs.Directory) (h linkHistory, err error) {
	hasHead, err := readRecord(d, headFile, &h.Head)
	if err != nil {
		return h, err
	}
	if hasHead && (h.Head.Version != 1 || h.Head.Sequence < 1 || h.Head.Sequence > maxLinks || !safefs.ValidID(h.Head.ID, "n") || !safefs.DigestPattern.MatchString(h.Head.Hash)) {
		return h, fmt.Errorf("invalid Signal link head")
	}
	q, err := d.Sub(historyDirectory)
	if os.IsNotExist(err) && !hasHead {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	defer q.Close()
	names, err := q.File().Readdirnames(4*maxLinks + 33)
	if err != nil && !errors.Is(err, io.EOF) {
		return h, err
	}
	files := map[string]bool{}
	var intents []string
	temps := 0
	for _, name := range names {
		if safefs.IsJSONTemporary(name) {
			temps++
			continue
		}
		files[name] = true
		if strings.HasSuffix(name, ".intent.json") {
			intents = append(intents, name)
		}
	}
	if len(files) > 4*maxLinks || len(intents) > maxLinks || temps > 32 {
		return h, fmt.Errorf("Signal link history exceeds budget")
	}
	slices.Sort(intents)
	previous := ""
	var lastTime time.Time
	ids := map[string]bool{}
	for i, name := range intents {
		if name != recordFile(i+1, "intent") {
			return h, fmt.Errorf("Signal link history has a gap")
		}
		var r linkRecord
		if _, err = readRecord(q, name, &r.Intent); err != nil {
			return h, err
		}
		delete(files, name)
		v := r.Intent
		if v.Version != 1 || v.Sequence != i+1 || !safefs.ValidID(v.ID, "n") || ids[v.ID] || !protocol.ValidToken(v.Name) || v.Previous != previous || v.CreatedAt.IsZero() || v.CreatedAt.Before(lastTime) {
			return h, fmt.Errorf("invalid Signal link intent")
		}
		ids[v.ID] = true
		lastTime = v.CreatedAt
		start := linkStart{}
		present, e := readRecord(q, recordFile(i+1, "start"), &start)
		if e != nil {
			return h, e
		}
		if present {
			if start.Intent != protocol.Hash(v) || start.At.Before(v.CreatedAt) {
				return h, fmt.Errorf("invalid Signal link launch evidence")
			}
			r.Start = &start
			delete(files, recordFile(i+1, "start"))
			lastTime = start.At
		}
		result := linkResult{}
		present, e = readRecord(q, recordFile(i+1, "result"), &result)
		if e != nil {
			return h, e
		}
		if present {
			if r.Start == nil || result.Intent != protocol.Hash(v) || result.Start != protocol.Hash(start) || result.At.Before(start.At) || (result.Outcome != "linked" && result.Outcome != "uncertain") {
				return h, fmt.Errorf("invalid Signal link result")
			}
			r.Result = &result
			delete(files, recordFile(i+1, "result"))
			lastTime = result.At
		}
		resolution := linkResolution{}
		present, e = readRecord(q, recordFile(i+1, "resolution"), &resolution)
		if e != nil {
			return h, e
		}
		if present {
			if (r.Result != nil && r.Result.Outcome == "linked") || resolution.Intent != protocol.Hash(v) || resolution.Start != protocol.Hash(r.Start) || resolution.Result != protocol.Hash(r.Result) || !validReason(resolution.Reason) || resolution.At.Before(lastTime) {
				return h, fmt.Errorf("invalid Signal link review")
			}
			r.Resolution = &resolution
			delete(files, recordFile(i+1, "resolution"))
			lastTime = resolution.At
		}
		if i < len(intents)-1 && !r.complete() {
			return h, fmt.Errorf("Signal link superseded unfinished maintenance")
		}
		if v.Sequence == h.Head.Sequence && (h.Head.ID != v.ID || h.Head.Hash != protocol.Hash(v)) {
			return h, fmt.Errorf("Signal link head changed")
		}
		h.Records = append(h.Records, r)
		previous = protocol.Hash(r)
	}
	if len(files) != 0 || len(h.Records) < h.Head.Sequence || len(h.Records) > h.Head.Sequence+1 {
		return h, fmt.Errorf("Signal link history and head disagree")
	}
	h.Pending = len(h.Records) > h.Head.Sequence
	if h.Pending && h.Records[len(h.Records)-1].Start != nil {
		return h, fmt.Errorf("Signal link started without committed head")
	}
	return h, nil
}

// Status contains no linking URI, phone number, incoming message or CLI output.
type Status struct {
	RecordHash     string    `json:"record_hash,omitempty"`
	Operation      string    `json:"operation,omitempty"`
	Sequence       int       `json:"sequence"`
	State          string    `json:"state"`
	RuntimeAllowed bool      `json:"runtime_allowed"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
}

func (h linkHistory) status() Status {
	out := Status{State: "none", RuntimeAllowed: h.runtimeAllowed()}
	if len(h.Records) == 0 {
		return out
	}
	r := h.Records[len(h.Records)-1]
	out.RecordHash = protocol.Hash(r)
	out.Operation, out.Sequence, out.State = r.Intent.ID, r.Intent.Sequence, "prepared"
	if r.Start != nil {
		out.State = "uncertain"
		out.StartedAt = r.Start.At
	}
	if r.Result != nil {
		out.State = r.Result.Outcome
		out.FinishedAt = r.Result.At
	}
	if r.Resolution != nil {
		out.State = "reviewed"
		out.FinishedAt = r.Resolution.At
	}
	if h.Pending {
		out.State = "publication_pending"
	}
	return out
}

// Inspect requires the account lease: a surviving CLI child remains busy even if
// its parent died. This read never resumes a link or clears a maintenance fence.
func Inspect(path string) (Status, error) {
	d, guard, err := control(path, false)
	if err != nil {
		return Status{}, err
	}
	defer d.Close()
	defer guard.Close()
	// An enrolled recovery lease must not be recreated by a diagnostic open.
	if _, err = readRecovery(d); err != nil {
		return Status{}, err
	}
	l, err := acquire(path)
	if err != nil {
		return Status{}, err
	}
	defer l.Close()
	h, err := readHistory(l.dir)
	if err != nil {
		return Status{}, err
	}
	s := h.status()
	err = applyMaintenanceStatus(d, &s)
	return s, err
}

func applyMaintenanceStatus(d *safefs.Directory, s *Status) error {
	r, err := readRecovery(d)
	if err != nil {
		s.RuntimeAllowed = false
		return err
	}
	s.RuntimeAllowed = s.RuntimeAllowed && !r.HasIntent
	h, err := readMaintenance(d)
	if err != nil {
		s.RuntimeAllowed = false
		return err
	}
	s.RuntimeAllowed = s.RuntimeAllowed && h.runtimeAllowed()
	links, err := readHistory(d)
	if err == nil {
		err = maintenanceLinkEvidence(h, links)
	}
	if err != nil {
		s.RuntimeAllowed = false
	}
	return err
}
