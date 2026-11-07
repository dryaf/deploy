package workerctl

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"syscall"

	"github.com/dryaf/deploy/internal/safefs"
)

// OperationPins are fixed before the first child transition. A retry cannot
// silently retarget a different UID, installation, artifact or desired policy.
type OperationPins struct {
	DesiredScope      int               `json:"desired_scope,omitempty"`
	PreviousOperation string            `json:"previous_operation,omitempty"`
	Previous          string            `json:"previous,omitempty"`
	Worker            string            `json:"worker"`
	Action            string            `json:"action"`
	DesiredHash       string            `json:"desired_hash"`
	UID               int               `json:"uid"`
	Home              string            `json:"home"`
	Installations     map[string]string `json:"installations"`
	Artifacts         map[string]string `json:"artifacts"`
}

type ChildReference struct {
	Name      string `json:"name"`
	Operation string `json:"operation"`
	Complete  bool   `json:"complete"`
	Receipt   string `json:"receipt,omitempty"`
}

type OperationJournal struct {
	Version  int              `json:"version"`
	ID       string           `json:"id"`
	Pins     OperationPins    `json:"pins"`
	Children []ChildReference `json:"children"`
	Complete bool             `json:"complete"`
}

// Inspect must consult canonical child evidence. Unknown outcomes are errors,
// never "absent". Run must implement an idempotent, journaled domain transition
// using the supplied operation ID; this journal is not a replacement for it.
type LifecycleStep interface {
	Name() string
	Inspect(context.Context, string) (receipt string, complete bool, err error)
	Run(context.Context, string) error
}

type JournalCoordinator struct {
	Directory *safefs.Directory
	Boundary  func(string) error
}

func (j JournalCoordinator) boundary(name string) error {
	if j.Boundary != nil {
		return j.Boundary(name)
	}
	return nil
}

func (j JournalCoordinator) Apply(ctx context.Context, pins OperationPins, steps []LifecycleStep) (OperationJournal, error) {
	var record OperationJournal
	if pins.DesiredScope != 0 && (pins.DesiredScope != 1 || pins.Worker != "notification-worker") {
		return record, fmt.Errorf("unsupported desired policy scope")
	}
	if j.Directory == nil {
		return record, fmt.Errorf("private coordinator directory required")
	}
	info, statErr := j.Directory.File().Stat()
	if statErr != nil {
		return record, statErr
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
		return record, fmt.Errorf("coordinator directory must be owned and private")
	}
	if pins.PreviousOperation != "" && (!safefs.ValidID(pins.PreviousOperation, "n") || !validDigest(pins.Previous)) {
		return record, fmt.Errorf("invalid predecessor operation pin")
	}
	if pins.UID != os.Getuid() || pins.UID <= 0 || !validDigest(pins.DesiredHash) || !validWorkerKind(pins.Worker) || !isShortOperation(pins.Action) {
		return record, fmt.Errorf("invalid coordinator target or policy pins")
	}
	if _, err := ResolveTargetPath(pins.Home, pins.Home); err != nil {
		return record, err
	}
	for _, hash := range pins.Artifacts {
		if !validDigest(hash) {
			return record, fmt.Errorf("artifact pin requires exact digest")
		}
	}
	if len(steps) == 0 || len(steps) > 64 {
		return record, fmt.Errorf("bounded nonempty lifecycle required")
	}
	seen := map[string]bool{}
	for _, step := range steps {
		if step == nil || !safeStepName(step.Name()) || seen[step.Name()] {
			return record, fmt.Errorf("invalid lifecycle step")
		}
		seen[step.Name()] = true
	}
	claimed := false
	if file, openErr := j.Directory.Open("operation.lock", os.O_RDONLY, 0); openErr == nil {
		claimed = true
		file.Close()
	} else if !os.IsNotExist(openErr) {
		return record, fmt.Errorf("coordinator lock unavailable; recovery required")
	}
	if !claimed {
		if file, openErr := j.Directory.Open("operation.json", os.O_RDONLY, 0); openErr == nil {
			file.Close()
			return record, fmt.Errorf("coordinator lock missing from existing operation; recovery required")
		} else if !os.IsNotExist(openErr) {
			return record, openErr
		}
	}
	lock, err := j.Directory.Lock("operation.lock", true, true)
	if err != nil {
		return record, fmt.Errorf("worker operation busy or unavailable: %w", err)
	}
	defer lock.Close()
	exists := true
	if err = j.Directory.ReadJSON("operation.json", &record); os.IsNotExist(err) {
		if claimed {
			return record, fmt.Errorf("coordinator journal missing from claimed installation; recovery required")
		}
		exists = false
	} else if err != nil {
		return record, fmt.Errorf("coordinator journal unavailable; recovery required")
	}
	replaceCompleted := false
	if exists && !reflect.DeepEqual(record.Pins, pins) {
		if err = validateOperationJournal(record); err != nil {
			return record, err
		}
		if pins.PreviousOperation != "" && pins.PreviousOperation != record.ID || !record.Complete || pins.Previous != record.Digest() || record.Pins.UID != pins.UID || record.Pins.Home != pins.Home || record.Pins.Worker != pins.Worker {
			return record, fmt.Errorf("conflict: changed policy requires the completed predecessor's exact receipt")
		}
		if err = j.Directory.Mkdir("history"); err != nil {
			return record, err
		}
		history := "history/" + record.ID + ".json"
		var retained OperationJournal
		if err = j.Directory.ReadJSON(history, &retained); os.IsNotExist(err) {
			err = j.Directory.WriteJSON(history, record, false)
		} else if err == nil && !reflect.DeepEqual(retained, record) {
			err = fmt.Errorf("coordinator history changed")
		}
		if err != nil {
			return record, err
		}
		record = OperationJournal{}
		exists = false
		replaceCompleted = true
	}
	if exists {
		if err = validateOperationJournal(record); err != nil {
			return record, err
		}
		if record.Version != 1 || !safefs.ValidID(record.ID, "n") || len(record.Children) != len(steps) {
			return record, fmt.Errorf("invalid coordinator journal; recovery required")
		}
		if !reflect.DeepEqual(record.Pins, pins) {
			return record, fmt.Errorf("conflict: pinned operation differs; finish or explicitly replan before applying changed YAML")
		}
		for i, child := range record.Children {
			if child.Name != steps[i].Name() || !safefs.ValidID(child.Operation, "n") || child.Complete && !validDigest(child.Receipt) || !child.Complete && child.Receipt != "" {
				return record, fmt.Errorf("coordinator child pins changed; recovery required")
			}
		}
	} else {
		record.Version = 1
		record.Pins = pins
		record.ID, err = safefs.NewID("n")
		if err != nil {
			return record, err
		}
		for _, step := range steps {
			id, err := safefs.NewID("n")
			if err != nil {
				return record, err
			}
			record.Children = append(record.Children, ChildReference{Name: step.Name(), Operation: id})
		}
		if err = j.Directory.WriteJSON("operation.json", record, replaceCompleted); err != nil {
			return record, err
		}
		if err = j.boundary("intent"); err != nil {
			return record, err
		}
	}
	for i, step := range steps {
		if err = ctx.Err(); err != nil {
			return record, err
		}
		child := &record.Children[i]
		receipt, complete, inspectErr := step.Inspect(ctx, child.Operation)
		if inspectErr != nil {
			return record, inspectErr
		}
		if child.Complete {
			if !complete || receipt != child.Receipt {
				return record, fmt.Errorf("canonical child evidence changed; recovery required")
			}
			continue
		}
		if record.Complete {
			return record, fmt.Errorf("completed coordinator contains unfinished child")
		}
		if !complete {
			if err = step.Run(ctx, child.Operation); err != nil {
				return record, err
			}
			if err = j.boundary("ran/" + child.Name); err != nil {
				return record, err
			}
			receipt, complete, err = step.Inspect(ctx, child.Operation)
			if err != nil {
				return record, err
			}
		}
		if !complete || !validDigest(receipt) {
			return record, fmt.Errorf("child transition has no canonical completion receipt")
		}
		child.Complete = true
		child.Receipt = receipt
		if err = j.Directory.WriteJSON("operation.json", record, true); err != nil {
			return record, err
		}
		if err = j.boundary("recorded/" + child.Name); err != nil {
			return record, err
		}
	}
	if !record.Complete {
		record.Complete = true
		if err = j.Directory.WriteJSON("operation.json", record, true); err != nil {
			return record, err
		}
		if err = j.boundary("complete"); err != nil {
			return record, err
		}
	}
	return record, nil
}
func validWorkerKind(s string) bool { return s == "backup-worker" || s == "notification-worker" }
func safeStepName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Marshal deliberately has no secret-bearing inputs. Child journal IDs provide
// reconciliation references; credential fingerprints remain domain-private.
func (j OperationJournal) Marshal() ([]byte, error) { return json.Marshal(j) }

func (j OperationJournal) Digest() string {
	body, _ := json.Marshal(j)
	return fmt.Sprintf("%x", sha256.Sum256(body))
}
func validateOperationJournal(j OperationJournal) error {
	if j.Pins.DesiredScope != 0 && (j.Pins.DesiredScope != 1 || j.Pins.Worker != "notification-worker") {
		return fmt.Errorf("unsupported retained policy scope")
	}
	if j.Pins.PreviousOperation != "" && (!safefs.ValidID(j.Pins.PreviousOperation, "n") || !validDigest(j.Pins.Previous)) {
		return fmt.Errorf("invalid predecessor journal pin")
	}

	if j.Version != 1 || !safefs.ValidID(j.ID, "n") || !validDigest(j.Pins.DesiredHash) || len(j.Children) == 0 || len(j.Children) > 64 {
		return fmt.Errorf("invalid coordinator journal; recovery required")
	}
	names := map[string]bool{}
	ids := map[string]bool{}
	for _, child := range j.Children {
		if !safeStepName(child.Name) || names[child.Name] || !safefs.ValidID(child.Operation, "n") || ids[child.Operation] || child.Complete && !validDigest(child.Receipt) || !child.Complete && child.Receipt != "" || j.Complete && !child.Complete {
			return fmt.Errorf("invalid coordinator child; recovery required")
		}
		names[child.Name] = true
		ids[child.Operation] = true
	}
	return nil
}
