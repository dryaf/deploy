package signalaccount

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const maintenanceDirectory = "deploy-service-link-history"
const maintenanceHeadFile = "deploy-service-link-head.json"

var maintenancePhases = []string{"prepared", "stopped", "settled", "released", "complete"}

type maintenanceRecord struct {
	Version, Sequence         int
	Operation, Name, Binding  string
	BeforeActive              bool
	Phase, Previous, LinkHash string
	At                        time.Time
}
type maintenanceHead struct {
	Sequence    int
	Phase, Hash string
}
type maintenanceHistory struct {
	Records []maintenanceRecord
	Head    maintenanceHead
	Pending bool
}

func maintenanceFile(r maintenanceRecord) string {
	return fmt.Sprintf("%04d.%d.json", r.Sequence, slices.Index(maintenancePhases, r.Phase))
}
func (h maintenanceHistory) last() *maintenanceRecord {
	if len(h.Records) == 0 {
		return nil
	}
	return &h.Records[len(h.Records)-1]
}
func (h maintenanceHistory) runtimeAllowed() bool {
	r := h.last()
	return !h.Pending && (r == nil || r.Phase == "released" || r.Phase == "complete")
}

// Every phase is immutable and hash-linked. The head may lag by exactly one
// durable record; that state always fences startup until explicit resume.
func readMaintenance(d *safefs.Directory) (h maintenanceHistory, err error) {
	hasHead, err := readRecord(d, maintenanceHeadFile, &h.Head)
	if err != nil {
		return h, err
	}
	if hasHead && (h.Head.Sequence < 1 || h.Head.Sequence > maxLinks || !slices.Contains(maintenancePhases, h.Head.Phase) || !safefs.DigestPattern.MatchString(h.Head.Hash)) {
		return h, fmt.Errorf("invalid Signal maintenance head")
	}
	q, err := d.Sub(maintenanceDirectory)
	if os.IsNotExist(err) && !hasHead {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	defer q.Close()
	names, err := q.File().Readdirnames(maxLinks*len(maintenancePhases) + 33)
	if err != nil && !errors.Is(err, io.EOF) {
		return h, err
	}
	temps := 0
	names = slices.DeleteFunc(names, func(name string) bool {
		if safefs.IsJSONTemporary(name) {
			temps++
			return true
		}
		return false
	})
	if len(names) > maxLinks*len(maintenancePhases) || temps > 32 {
		return h, fmt.Errorf("Signal maintenance history exceeds budget")
	}
	slices.Sort(names)
	previous := ""
	sequence, phase := 1, 0
	headPosition := -1
	ids := map[string]bool{}
	var first maintenanceRecord
	for i, name := range names {
		var r maintenanceRecord
		if _, err = readRecord(q, name, &r); err != nil {
			return h, err
		}
		if r.Version != 1 || r.Sequence != sequence || r.Phase != maintenancePhases[phase] || name != maintenanceFile(r) || !safefs.ValidID(r.Operation, "n") || !protocol.ValidToken(r.Name) || !safefs.DigestPattern.MatchString(r.Binding) || r.Previous != previous || r.At.IsZero() {
			return h, fmt.Errorf("invalid Signal maintenance record")
		}
		if i > 0 && r.At.Before(h.Records[i-1].At) {
			return h, fmt.Errorf("Signal maintenance time regressed")
		}
		if phase == 0 {
			if ids[r.Operation] {
				return h, fmt.Errorf("Signal maintenance operation ID reused")
			}
			ids[r.Operation] = true
			first = r
		} else if r.Operation != first.Operation || r.Name != first.Name || r.Binding != first.Binding || r.BeforeActive != first.BeforeActive {
			return h, fmt.Errorf("Signal maintenance authority changed")
		}
		if phase < 2 && r.LinkHash != "" || phase >= 2 && !safefs.DigestPattern.MatchString(r.LinkHash) {
			return h, fmt.Errorf("Signal maintenance outcome evidence missing or premature")
		}
		if phase > 2 && r.LinkHash != h.Records[i-1].LinkHash {
			return h, fmt.Errorf("Signal maintenance outcome changed")
		}
		previous = protocol.Hash(r)
		h.Records = append(h.Records, r)
		if hasHead && h.Head.Sequence == sequence && h.Head.Phase == r.Phase {
			if h.Head.Hash != previous {
				return h, fmt.Errorf("Signal maintenance head digest mismatch")
			}
			headPosition = i
		}
		phase++
		if phase == len(maintenancePhases) {
			phase = 0
			sequence++
		}
	}
	if hasHead && headPosition < 0 || len(h.Records) > headPosition+2 {
		return h, fmt.Errorf("Signal maintenance history and head diverged")
	}
	h.Pending = len(h.Records) == headPosition+2
	return h, nil
}

func acknowledgeMaintenance(d *safefs.Directory, h maintenanceHistory) error {
	r := h.last()
	if r == nil || !h.Pending {
		return nil
	}
	return writeRecord(d, maintenanceHeadFile, maintenanceHead{r.Sequence, r.Phase, protocol.Hash(*r)}, h.Head.Sequence != 0)
}

func writeMaintenance(d *safefs.Directory, h maintenanceHistory, r maintenanceRecord, fault func(string) error) error {
	if h.Pending {
		return fmt.Errorf("acknowledge the pending maintenance phase first")
	}
	if err := d.Capacity(64 << 10); err != nil {
		return err
	}
	if err := d.Mkdir(maintenanceDirectory); err != nil {
		return err
	}
	if err := writeRecord(d, maintenanceDirectory+"/"+maintenanceFile(r), r, false); err != nil {
		return err
	}
	if fault != nil {
		if err := fault("maintenance_" + r.Phase + "_record"); err != nil {
			return err
		}
	}
	if err := writeRecord(d, maintenanceHeadFile, maintenanceHead{r.Sequence, r.Phase, protocol.Hash(r)}, h.Head.Sequence != 0); err != nil {
		return err
	}
	if fault != nil {
		return fault("maintenance_" + r.Phase + "_head")
	}
	return nil
}

func linkMaintenanceAllowed(d *safefs.Directory, operation, name string) error {
	h, err := readMaintenance(d)
	if err != nil {
		return err
	}
	r := h.last()
	if h.Pending {
		return fmt.Errorf("Signal service maintenance publication needs resume")
	}
	if r == nil || r.Phase == "complete" {
		return nil
	}
	if r.Operation != operation || r.Name != name || r.Phase == "prepared" {
		return fmt.Errorf("Signal link is not authorized by stopped service maintenance")
	}
	return nil
}

func maintenanceLinkEvidence(h maintenanceHistory, links linkHistory) error {
	r := h.last()
	if r == nil || slices.Index(maintenancePhases, r.Phase) < 2 {
		return nil
	}
	for _, link := range links.Records {
		if link.Intent.ID == r.Operation {
			if link.Intent.Name != r.Name || !link.complete() || protocol.Hash(link) != r.LinkHash {
				return fmt.Errorf("Signal maintenance lost its exact account outcome")
			}
			return nil
		}
	}
	return fmt.Errorf("Signal maintenance account outcome is missing")
}

// No external service state or raw image/account credentials are exposed here.
type MaintenanceStatus struct {
	Operation      string `json:"operation,omitempty"`
	Phase          string `json:"phase"`
	Binding        string `json:"binding,omitempty"`
	BeforeActive   bool   `json:"previously_active"`
	Pending        bool   `json:"publication_pending"`
	RuntimeAllowed bool   `json:"runtime_allowed"`
}

func (h maintenanceHistory) status() MaintenanceStatus {
	s := MaintenanceStatus{Phase: "none", Pending: h.Pending, RuntimeAllowed: h.runtimeAllowed()}
	if r := h.last(); r != nil {
		s.Operation, s.Phase, s.Binding, s.BeforeActive = r.Operation, r.Phase, r.Binding, r.BeforeActive
	}
	return s
}
func InspectMaintenance(path string) (MaintenanceStatus, error) {
	d, lock, err := control(path, false)
	if err != nil {
		return MaintenanceStatus{}, err
	}
	defer d.Close()
	defer lock.Close()
	h, err := readMaintenance(d)
	if err != nil {
		return MaintenanceStatus{}, err
	}
	links, err := readHistory(d)
	if err != nil {
		return MaintenanceStatus{}, err
	}
	if err = maintenanceLinkEvidence(h, links); err != nil {
		return MaintenanceStatus{}, err
	}
	s := h.status()
	r, err := readRecovery(d)
	if err != nil {
		return MaintenanceStatus{}, err
	}
	native, err := nativeServicePresent(d)
	if err != nil {
		return MaintenanceStatus{}, err
	}
	s.RuntimeAllowed = s.RuntimeAllowed && links.runtimeAllowed() && !r.HasIntent && !native
	return s, nil
}
