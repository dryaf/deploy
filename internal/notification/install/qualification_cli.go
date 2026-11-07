package install

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

type QualificationStatus struct {
	RecordHash        string               `json:"record_hash,omitempty"`
	Record            *QualificationRecord `json:"latest_record,omitempty"`
	Pending           bool                 `json:"acknowledgement_pending"`
	BindingMatches    bool                 `json:"binding_matches"`
	EvidenceComplete  bool                 `json:"evidence_complete"`
	Qualified         bool                 `json:"qualified"`
	ActivationAllowed bool                 `json:"activation_allowed"`
	Reason            string               `json:"reason"`
}

func qualificationInstallation(d *safefs.Directory, root string) (m ManagedManifest, c Runtime, err error) {
	j, err := readManagedJournal(d)
	if err != nil {
		return m, c, err
	}
	m = j.Intent.After
	if j.Phase != "complete" || (m.Mode != "verification" && m.Mode != "delivery") || m.Root != root {
		return m, c, fmt.Errorf("qualification requires a completed verification installation")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return
	}
	if c, err = verifyManagedArtifacts(d, m); err != nil {
		return
	}
	if m.DeliveryOwnerHash != "" {
		owner, e := worker.InspectManagedDeliveryOwner(m.Paths.State)
		if e != nil {
			return m, c, e
		}
		if owner != deliveryOwner(m, c) || protocol.Hash(owner) != m.DeliveryOwnerHash {
			return m, c, fmt.Errorf("managed delivery ownership changed")
		}
	}
	units, err := managedUnitDirectory(m.UnitDirectory)
	if err != nil {
		return m, c, err
	}
	defer units.Close()
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return m, c, err
	}
	if !present || managedDigest(unit) != m.UnitHash {
		return m, c, fmt.Errorf("qualification unit changed")
	}
	state, err := privateManagedDir(m.Paths.State, true)
	if err != nil {
		return m, c, err
	}
	defer state.Close()
	var identity protocol.Identity
	if err = state.ReadJSON("identity.json", &identity); err == nil && identity != m.Identity {
		err = fmt.Errorf("qualification worker identity changed")
	}
	return
}

func runQualificationCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Inspect this user's managed installation")
	root := f.String("root", "", "Existing verification installation root")
	reportPath := f.String("report", "", "Private operator qualification report JSON")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || !managedPath(*root) || f.NArg() != 0 {
		return fmt.Errorf("qualification requires --local and an absolute --root")
	}
	bad := false
	f.Visit(func(v *flag.Flag) {
		if v.Name == "report" && args[0] != "qualify" {
			bad = true
		}
	})
	if bad || (args[0] == "qualify" && !managedPath(*reportPath)) {
		return fmt.Errorf("--report is required only for qualify")
	}
	var report QualificationReport
	if args[0] == "qualify" {
		data, err := readManagedInput(*reportPath, 1<<20)
		if err != nil {
			return err
		}
		if err = safefs.DecodeJSON(data, &report, 1<<20); err != nil {
			return err
		}
	}
	d, err := privateManagedDir(*root, true)
	if err != nil {
		return err
	}
	defer d.Close()
	lease, err := d.Lock("operation.lock", args[0] == "qualify" && !ui.DryRun, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	m, c, err := qualificationInstallation(d, *root)
	if err != nil {
		return err
	}
	bound := m
	if m.Mode == "delivery" {
		if args[0] != "qualification-status" {
			return fmt.Errorf("deactivate delivery before collecting or recording new qualification")
		}
		_, bound, err = readDeliveryAdmission(d, m, c)
		if err != nil {
			return err
		}
	}
	b, err := qualificationBinding(bound, c)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	h, err := readQualificationHistory(d, m.Installation)
	if err != nil {
		return err
	}
	switch args[0] {
	case "qualification-template":
		id, e := safefs.NewID("n")
		if e != nil {
			return e
		}
		report = QualificationReport{Version: 1, ID: id, Binding: b, CreatedAt: now, Operator: "operator", Checks: qualificationChecks(c)}
		if e = report.validate(); e != nil {
			return e
		}
		return json.NewEncoder(out).Encode(report)
	case "qualify":
		matched := false
		for _, old := range h.Records {
			if old.Report.ID == report.ID {
				if protocol.Hash(old.Report) != protocol.Hash(report) {
					return fmt.Errorf("qualification report ID already has different evidence")
				}
				matched = true
			}
		}
		// An exact retry acknowledges historical evidence, even if an update or
		// expiry made it stale. It cannot renew its time or qualify the new binding.
		if !matched {
			if err = report.matches(b, c, now); err != nil {
				return err
			}
		}
		if ui.DryRun {
			if !matched && (h.Pending || len(h.Records) >= maxQualificationRecords) {
				return fmt.Errorf("qualification history pending or full")
			}
			return json.NewEncoder(out).Encode(struct {
				DryRun    bool `json:"dry_run"`
				Qualified bool `json:"qualified"`
			}{true, false})
		}
		if _, err = appendQualification(d, report, now, nil); err != nil {
			return err
		}
		h, err = readQualificationHistory(d, m.Installation)
		if err != nil {
			return err
		}
	case "qualification-status":
	default:
		return fmt.Errorf("unknown qualification operation")
	}
	status := qualificationStatusAt(h, b, c, now)
	return json.NewEncoder(out).Encode(status)
}
