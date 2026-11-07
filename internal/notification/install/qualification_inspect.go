package install

import (
	"context"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// QualificationSummary is safe for ordinary status output. Private reports
// retain credential hashes, artifact pins and evidence; none are exposed here.
type QualificationSummary struct {
	EvidenceComplete       bool                        `json:"evidence_complete"`
	AdmissionValid         bool                        `json:"admission_valid"`
	AcknowledgementPending bool                        `json:"acknowledgement_pending"`
	BindingMatches         bool                        `json:"binding_matches"`
	Checks                 []QualificationCheckSummary `json:"checks"`
	Reason                 string                      `json:"reason"`
}

type QualificationCheckSummary struct {
	ID     string `json:"id"`
	Scope  string `json:"scope"`
	Result string `json:"result"`
}

// InspectQualification never records, renews or acknowledges evidence. The
// manifest pin prevents a status caller from mixing two installation revisions.
func InspectQualification(ctx context.Context, root, manifestHash string) (QualificationSummary, error) {
	return inspectQualificationAt(ctx, root, manifestHash, time.Now().UTC())
}

func inspectQualificationAt(ctx context.Context, root, manifestHash string, now time.Time) (out QualificationSummary, err error) {
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if !managedPath(root) || !safefs.DigestPattern.MatchString(manifestHash) {
		return out, fmt.Errorf("qualification inspection requires exact installation pins")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return out, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return out, err
	}
	defer lock.Close()
	m, c, err := qualificationInstallation(d, root)
	if err != nil {
		return out, err
	}
	if protocol.Hash(m) != manifestHash {
		return out, fmt.Errorf("qualification installation revision changed")
	}
	bound := m
	if m.Mode == "delivery" {
		_, bound, err = readDeliveryAdmission(d, m, c)
		if err != nil {
			return out, err
		}
	}
	b, err := qualificationBinding(bound, c)
	if err != nil {
		return out, err
	}
	history, err := readQualificationHistory(d, m.Installation)
	if err != nil {
		return out, err
	}
	status := qualificationStatusAt(history, b, c, now)
	out.EvidenceComplete, out.AcknowledgementPending, out.BindingMatches, out.Reason = status.EvidenceComplete, status.Pending, status.BindingMatches, status.Reason
	checks := qualificationChecks(c)
	if status.Record != nil && status.BindingMatches && !status.Pending {
		checks = status.Record.Report.Checks
	}
	for _, check := range checks {
		out.Checks = append(out.Checks, QualificationCheckSummary{check.ID, check.Scope, check.Result})
	}
	if m.Mode == "delivery" && status.EvidenceComplete {
		out.AdmissionValid = checkDeliveryAdmission(d, m, c, now) == nil
	}
	return out, ctx.Err()
}

func qualificationStatusAt(h qualificationHistory, b QualificationBinding, c Runtime, now time.Time) QualificationStatus {
	status := QualificationStatus{Pending: h.Pending, Reason: "no qualification evidence recorded"}
	if len(h.Records) > 0 {
		record := h.Records[len(h.Records)-1]
		status.Record = &record
		status.RecordHash = protocol.Hash(record)
		matchErr := record.Report.matches(b, c, now)
		status.BindingMatches = matchErr == nil && !now.Before(record.RecordedAt)
		status.EvidenceComplete = !h.Pending && status.BindingMatches && record.Report.evidenceComplete(c)
		status.Reason = "operator evidence; service activation and producer admission are separate"
		if h.Pending {
			status.Reason = "audit acknowledgement pending; retry the exact report"
		} else if matchErr != nil {
			status.Reason = matchErr.Error()
		} else if now.Before(record.RecordedAt) {
			status.Reason = "qualification clock moved backward"
		}
	}
	return status
}
