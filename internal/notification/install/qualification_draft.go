package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// QualificationSession is the redacted view used by the guided client. The
// report's private input hashes and full manifest remain on the target.
type QualificationSession struct {
	LEDEnvironmentRequired bool                        `json:"led_environment_required,omitempty"`
	AcknowledgementPending bool                        `json:"acknowledgement_pending"`
	ID                     string                      `json:"id"`
	CreatedAt              time.Time                   `json:"created_at"`
	Recorded               bool                        `json:"recorded"`
	EvidenceComplete       bool                        `json:"evidence_complete"`
	Checks                 []QualificationCheckSummary `json:"checks"`
}
type QualificationObservation struct {
	Session       string `json:"session"`
	Scope         string `json:"scope"`
	ID            string `json:"id"`
	Result        string `json:"result"`
	EvidenceHash  string `json:"evidence_hash"`
	LEDVersion    string `json:"led_version,omitempty"`
	LEDConfigHash string `json:"led_config_sha256,omitempty"`
}
type qualificationDraft struct {
	DeliveryPause string              `json:"delivery_pause,omitempty"`
	Previous      string              `json:"previous,omitempty"`
	Version       int                 `json:"version"`
	Report        QualificationReport `json:"report"`
}

// BeginQualification resumes unrecorded observations, or starts a new draft
// from the latest current report. Reusing current observations preserves their
// original report window; beginning a wizard never renews stale qualification.
func BeginQualification(ctx context.Context, root, manifestHash string) (QualificationSession, error) {
	return qualificationDraftOperation(ctx, root, manifestHash, "begin", QualificationObservation{}, time.Now().UTC())
}
func ObserveQualification(ctx context.Context, root, manifestHash string, observation QualificationObservation) (QualificationSession, error) {
	return qualificationDraftOperation(ctx, root, manifestHash, "observe", observation, time.Now().UTC())
}
func CommitQualification(ctx context.Context, root, manifestHash, session string) (QualificationSession, error) {
	return qualificationDraftOperation(ctx, root, manifestHash, "commit", QualificationObservation{Session: session}, time.Now().UTC())
}

func qualificationDraftOperation(ctx context.Context, root, manifestHash, action string, observation QualificationObservation, now time.Time) (out QualificationSession, err error) {
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if !managedPath(root) || !safefs.DigestPattern.MatchString(manifestHash) || action != "begin" && action != "observe" && action != "commit" {
		return out, fmt.Errorf("qualification requires an exact verification installation")
	}
	if action == "begin" && observation != (QualificationObservation{}) || action == "commit" && (observation.Scope != "" || observation.ID != "" || observation.Result != "" || observation.EvidenceHash != "" || observation.LEDVersion != "" || observation.LEDConfigHash != "") {
		return out, fmt.Errorf("unexpected qualification observation fields")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return out, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", true, false)
	if err != nil {
		return out, err
	}
	defer lock.Close()
	m, c, err := qualificationInstallation(d, root)
	if err != nil {
		return out, err
	}
	if m.Mode != "verification" || protocol.Hash(m) != manifestHash {
		return out, fmt.Errorf("finish or deactivate the current delivery lifecycle before qualification")
	}
	binding, err := qualificationBinding(m, c)
	if err != nil {
		return out, err
	}
	pauseID, priorQualification, err := qualificationPause(d, m, c)
	if err != nil {
		return out, err
	}
	h, err := readQualificationHistory(d, m.Installation)
	if err != nil {
		return out, err
	}
	var draft qualificationDraft
	err = d.ReadJSON("qualification-draft.json", &draft)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return out, err
	}
	if exists && (draft.Version != 1 || draft.Report.validate() != nil) {
		return out, fmt.Errorf("qualification draft is corrupt; review retained evidence")
	}
	var recorded *QualificationRecord
	for i := range h.Records {
		if h.Records[i].Report.ID == draft.Report.ID {
			if protocol.Hash(h.Records[i].Report) != protocol.Hash(draft.Report) {
				return out, fmt.Errorf("qualification draft differs from recorded observations")
			}
			recorded = &h.Records[i]
		}
	}
	if action == "begin" {
		if h.Pending {
			// A record published before its head must be acknowledged exactly.
			// Do not select or create a replacement draft while it is pending.
			draft = qualificationDraft{DeliveryPause: pauseID, Version: 1, Previous: h.Records[len(h.Records)-1].Previous, Report: h.Records[len(h.Records)-1].Report}
			if err = draft.Report.matches(binding, c, now); err != nil {
				return out, fmt.Errorf("pending qualification audit requires the exact specialist acknowledgement")
			}
		} else if !exists || draft.DeliveryPause != pauseID || recorded != nil || draft.Previous != h.Head.Hash || draft.Report.matches(binding, c, now) != nil {
			if exists && recorded == nil {
				// Keep stale uncommitted observations for diagnosis; never move
				// them onto the new runtime/credential binding.
				if err = d.Mkdir("qualification-drafts"); err != nil {
					return out, err
				}
				name := "qualification-drafts/" + draft.Report.ID + ".json"
				var prior qualificationDraft
				e := d.ReadJSON(name, &prior)
				if os.IsNotExist(e) {
					archive, openErr := d.Sub("qualification-drafts")
					if openErr != nil {
						return out, openErr
					}
					names, listErr := archive.File().Readdirnames(maxQualificationRecords + 1)
					archive.Close()
					if listErr != nil && !errors.Is(listErr, io.EOF) {
						return out, listErr
					}
					if len(names) >= maxQualificationRecords {
						return out, fmt.Errorf("qualification draft history full; review retained evidence")
					}
					e = d.WriteJSON(name, draft, false)
				} else if e == nil && protocol.Hash(prior) != protocol.Hash(draft) {
					e = fmt.Errorf("retained qualification draft changed")
				}
				if e != nil {
					return out, e
				}
			}
			report := QualificationReport{Version: 1, Binding: binding, CreatedAt: now, Operator: "guided-operator", Checks: qualificationChecks(c)}
			if m.FreshSignal != nil {
				service, e := validateFreshSignalGateway(ctx, *m.FreshSignal, m.Root, m.Paths.State, m.UnitDirectory, c, false)
				if e != nil {
					return out, e
				}
				report.Environment.SignalCLISHA256 = service.Spec.CLISHA256
			}
			if len(h.Records) > 0 {
				latest := h.Records[len(h.Records)-1]
				if protocol.Hash(latest) != priorQualification && latest.Report.matches(binding, c, now) == nil && !now.Before(latest.RecordedAt) {
					if latest.Report.evidenceComplete(c) {
						return qualificationSession(latest.Report, true, true), nil
					}
					report = latest.Report // Preserve passing timestamps and their report window.
					report.Checks = append([]QualificationCheck(nil), report.Checks...)
					for i := range report.Checks {
						if report.Checks[i].Result == "failed" {
							report.Checks[i].Result, report.Checks[i].EvidenceHash, report.Checks[i].ObservedAt = "pending", "", time.Time{}
						}
					}
				}
			}
			if len(h.Records) >= maxQualificationRecords {
				return out, fmt.Errorf("qualification history full; review retained audit")
			}
			report.ID, err = safefs.NewID("n")
			if err != nil {
				return out, err
			}
			draft = qualificationDraft{DeliveryPause: pauseID, Version: 1, Previous: h.Head.Hash, Report: report}
		}
		if err = draft.Report.matches(binding, c, now); err != nil {
			return out, err
		}
		if err = d.WriteJSON("qualification-draft.json", draft, exists); err != nil {
			return out, err
		}
		out = qualificationSession(draft.Report, false, draft.Report.evidenceComplete(c))
		out.AcknowledgementPending = h.Pending
		return out, nil
	}
	if !exists || draft.DeliveryPause != pauseID || !safefs.ValidID(observation.Session, "n") || observation.Session != draft.Report.ID {
		return out, fmt.Errorf("qualification session changed; resume its current draft")
	}
	if action == "commit" && recorded != nil {
		// An exact retry may acknowledge stale historical evidence but cannot
		// renew its time, change its binding or qualify the current runtime.
		if _, err = appendQualification(d, draft.Report, now, nil); err != nil {
			return out, err
		}
		return recordedQualificationSession(d, draft.Report, binding, c, now)
	}
	if recorded != nil || h.Pending || draft.Previous != h.Head.Hash {
		return out, fmt.Errorf("qualification observations are immutable once recorded")
	}
	if err = draft.Report.matches(binding, c, now); err != nil {
		return out, fmt.Errorf("qualification draft expired or its installation inputs changed")
	}
	if action == "observe" && (observation.LEDVersion != "" || observation.LEDConfigHash != "") {
		if c.LEDSocket == "" || !protocol.ValidToken(observation.LEDVersion) || !safefs.DigestPattern.MatchString(observation.LEDConfigHash) || observation.Scope != "" || observation.ID != "" || observation.Result != "" || observation.EvidenceHash != "" {
			return out, fmt.Errorf("LED environment requires its observed version and configuration digest, separately from check results")
		}
		previous := draft.Report.Environment
		if previous.LEDVersion != "" || previous.LEDConfigHash != "" {
			if previous.LEDVersion != observation.LEDVersion || previous.LEDConfigHash != observation.LEDConfigHash {
				return out, fmt.Errorf("LED environment already recorded differently; review the retained report and daemon configuration")
			}
			return qualificationSession(draft.Report, false, draft.Report.evidenceComplete(c)), nil
		}
		draft.Report.Environment.LEDVersion = observation.LEDVersion
		draft.Report.Environment.LEDConfigHash = observation.LEDConfigHash
		if err = draft.Report.matches(binding, c, now); err != nil {
			return out, err
		}
		if err = d.WriteJSON("qualification-draft.json", draft, true); err != nil {
			return out, err
		}
		return qualificationSession(draft.Report, false, draft.Report.evidenceComplete(c)), nil
	}
	if action == "observe" {
		if observation.Result != "passed" && observation.Result != "failed" || !safefs.DigestPattern.MatchString(observation.EvidenceHash) {
			return out, fmt.Errorf("observed checks require a passed/failed result and evidence file digest")
		}
		found := false
		for i := range draft.Report.Checks {
			check := &draft.Report.Checks[i]
			if check.Scope != observation.Scope || check.ID != observation.ID {
				continue
			}
			found = true
			if check.Result != "pending" {
				if check.Result != observation.Result || check.EvidenceHash != observation.EvidenceHash {
					return out, fmt.Errorf("observation already has different evidence; record it before starting a new report")
				}
				return qualificationSession(draft.Report, false, draft.Report.evidenceComplete(c)), nil
			}
			check.Result, check.EvidenceHash, check.ObservedAt = observation.Result, observation.EvidenceHash, now
		}
		if !found {
			return out, fmt.Errorf("observation is outside the current qualification checks")
		}
		if err = draft.Report.matches(binding, c, now); err != nil {
			return out, err
		}
		if err = d.WriteJSON("qualification-draft.json", draft, true); err != nil {
			return out, err
		}
		return qualificationSession(draft.Report, false, draft.Report.evidenceComplete(c)), nil
	}
	if _, err = appendQualification(d, draft.Report, now, nil); err != nil {
		return out, err
	}
	return recordedQualificationSession(d, draft.Report, binding, c, now)
}

func qualificationSession(r QualificationReport, recorded, complete bool) QualificationSession {
	s := QualificationSession{ID: r.ID, CreatedAt: r.CreatedAt, Recorded: recorded, EvidenceComplete: complete}
	for _, check := range r.Checks {
		if check.Scope == "led" && (r.Environment.LEDVersion == "" || r.Environment.LEDConfigHash == "") {
			s.LEDEnvironmentRequired = true
		}
		s.Checks = append(s.Checks, QualificationCheckSummary{check.ID, check.Scope, check.Result})
	}
	return s
}

func recordedQualificationSession(d *safefs.Directory, report QualificationReport, binding QualificationBinding, c Runtime, now time.Time) (QualificationSession, error) {
	h, err := readQualificationHistory(d, binding.Manifest.Installation)
	if err != nil {
		return QualificationSession{}, err
	}
	status := qualificationStatusAt(h, binding, c, now)
	latest := status.Record != nil && status.Record.Report.ID == report.ID
	return qualificationSession(report, true, latest && status.EvidenceComplete), nil
}

// Each explicit pause starts a new observation cycle, even when its verification
// manifest is byte-for-byte identical to an earlier pause. Retained evidence is
// still auditable, but cannot serve as the next cycle's observations.
func qualificationPause(d *safefs.Directory, m ManagedManifest, c Runtime) (string, string, error) {
	j, err := readManagedJournal(d)
	if err != nil {
		return "", "", err
	}
	if j.Intent.Operation == "restore-shared-activity" && m.DeliveryOwnerHash == "" {
		return j.Intent.ID, "", nil
	}
	if j.Intent.Operation != "pause-delivery" && !isPausedRuntimeChange(j.Intent.Operation) && j.Intent.Operation != "restore-shared-activity" {
		return "", "", nil
	}
	if j.Phase != "complete" || !preservesManagedDeliveryOwner(j) || protocol.Hash(j.Intent.After) != protocol.Hash(m) {
		return "", "", fmt.Errorf("qualification requires its exact completed delivery pause")
	}
	prior := j
	if j.Intent.Operation == "restore-shared-activity" {
		prior, err = sharedActivityPause(d, j)
		if err != nil {
			return "", "", err
		}
	}
	if isPausedRuntimeChange(j.Intent.Operation) {
		prior, err = deliveryProviderPause(d, j, c)
		if err != nil {
			return "", "", err
		}
	}
	original, err := managedRuntime(d, *prior.Intent.Before)
	if err != nil {
		return "", "", err
	}
	proof, _, err := readDeliveryAdmission(d, *prior.Intent.Before, original)
	if err != nil {
		return "", "", err
	}
	return j.Intent.ID, protocol.Hash(proof.Qualification), nil
}
