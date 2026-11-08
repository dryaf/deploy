package workerctl

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

func isQualificationControl(action string) bool {
	return action == "qualify-begin" || action == "qualify-observe" || action == "qualify-commit"
}

type qualificationLifecycle struct {
	begin   func(context.Context, string, string) (install.QualificationSession, error)
	observe func(context.Context, string, string, install.QualificationObservation) (install.QualificationSession, error)
	commit  func(context.Context, string, string, string) (install.QualificationSession, error)
}

func defaultQualificationLifecycle() qualificationLifecycle {
	return qualificationLifecycle{install.BeginQualification, install.ObserveQualification, install.CommitQualification}
}

func qualifyNotificationControl(ctx context.Context, r ControlRequest, paths ResolvedPaths, lifecycle qualificationLifecycle) (ControlResult, error) {
	result := ControlResult{Version: 1}
	d, err := safefs.Open(paths.Control)
	if err != nil {
		return result, fmt.Errorf("apply and enroll real consumers before qualification")
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	var record OperationJournal
	if err = d.ReadJSON("operation.json", &record); err != nil {
		return result, err
	}
	if validateOperationJournal(record) != nil || !record.Complete || record.Pins.Action != "apply" || record.Pins.DesiredHash != r.DesiredHash || record.Pins.UID != r.Facts.UID || record.Pins.Home != r.Facts.Home || record.Pins.Artifacts["helper"] != r.HelperSHA256 || record.Pins.Installations["notification-worker"] != paths.Installation || !enrolledNotificationJournal(record) {
		return result, fmt.Errorf("qualification requires the unchanged completed consumer enrollment in verification mode")
	}
	if r.Desired.Notification.StatusLEDs.Enabled && len(r.Desired.Notification.StatusLEDs.Bindings) != 0 && !mappingJournal(record) && !enrolledOutputJournal(record) && !sharedLimitsJournal(record) && !requalificationJournal(record) && !commandPauseJournal(record) && !deliveryProviderJournal(record) && !sharedActivityJournal(record) {
		return result, fmt.Errorf("apply the declared LED bindings before qualification")
	}
	if sharedLimitsJournal(record) {
		if err = verifySharedLEDDeclaration(r, paths, d, record); err != nil {
			return result, err
		}
	}
	if mappingJournal(record) {
		if _, err = readMappingIntent(d, record); err != nil {
			return result, err
		}
	}
	if err = verifyMappingScope(d, record, nil); err != nil {
		return result, err
	}
	var session install.QualificationSession
	switch r.Action {
	case "qualify-begin":
		if r.Observation != nil {
			return result, fmt.Errorf("begin qualification does not accept observations")
		}
		session, err = lifecycle.begin(ctx, paths.Installation, record.Children[0].Receipt)
	case "qualify-observe":
		if r.Observation == nil {
			return result, fmt.Errorf("qualification observation missing")
		}
		session, err = lifecycle.observe(ctx, paths.Installation, record.Children[0].Receipt, *r.Observation)
	case "qualify-commit":
		if r.Observation == nil || r.Observation.Scope != "" || r.Observation.ID != "" || r.Observation.Result != "" || r.Observation.EvidenceHash != "" || r.Observation.LEDVersion != "" || r.Observation.LEDConfigHash != "" {
			return result, fmt.Errorf("commit requires only the retained qualification session")
		}
		session, err = lifecycle.commit(ctx, paths.Installation, record.Children[0].Receipt, r.Observation.Session)
	default:
		return result, fmt.Errorf("unknown qualification action")
	}
	if err != nil {
		return result, err
	}
	result.QualificationSession = &session
	result.Code, result.State = "ok", "qualification_draft"
	result.Next = "continue recording actual observations; pending checks remain unqualified"
	if session.Recorded {
		result.State = "qualification_recorded"
		result.Next = "complete the remaining observations, then run deploy notification-worker qualify " + r.Server
		if session.LEDEnvironmentRequired {
			result.Next = "record the observed LED daemon version and configuration file with deploy notification-worker qualify " + r.Server
		}
		if session.EvidenceComplete {
			result.Next = "run deploy notification-worker " + r.Server + " to admit delivery and connect production reporting using this qualification"
		}
	}
	return result, nil
}

func (c CLI) qualifyNotification(ctx context.Context, v Invocation, inv config.ServerInventory, facts TargetFacts, p Plan) error {
	return c.qualifyWorker(ctx, "notification-worker", v, inv, facts, p)
}

func (c CLI) qualifyWorker(ctx context.Context, kind string, v Invocation, inv config.ServerInventory, facts TargetFacts, p Plan) error {
	if p.Artifacts == nil {
		return fmt.Errorf("matching helper bundle required for guided qualification")
	}
	if c.Controller == nil {
		c.Controller = SSHController{}
	}
	prompt := c.Prompt
	if prompt == nil {
		prompt = TerminalPrompt{Input: os.Stdin, Output: c.Err}
	}
	r := ControlRequest{Version: 1, Worker: kind, Server: v.Server, Action: "qualify-begin", Facts: facts, Desired: *inv.Servers[v.Server].Desired, DesiredScope: p.DesiredScope, DesiredHash: p.DesiredHash}
	if kind == "notification-worker" {
		if signal, ok := r.Desired.Notification.Providers["signal"]; ok {
			r.SignalSHA256 = signal.ExecutableSHA256
			if signal.Executable == "" {
				if p.Artifacts.Signal == nil {
					return fmt.Errorf("matching native Signal artifact pin required for qualification")
				}
				r.SignalSHA256 = p.Artifacts.Signal.SHA256
			}
			if !validDigest(r.SignalSHA256) {
				return fmt.Errorf("valid native Signal artifact pin required for qualification")
			}
		}
	}
	if kind == "backup-worker" && (r.Desired.Backup.Notifications.Worker == "") {
		r.Desired.Notification = nil
	}
	if kind == "backup-worker" {
		var err error
		r.SourceTargets, err = declaredSourceTargets(inv, r.Desired)
		if err != nil {
			return err
		}
	}
	call := func() (ControlResult, error) {
		result, err := c.Controller.Apply(ctx, inv.Servers[v.Server], p.Artifacts.Helper, r)
		if err != nil {
			return result, err
		}
		if result.Version != 1 || result.Code != "ok" || result.QualificationSession == nil || !safefs.ValidID(result.QualificationSession.ID, "n") || len(result.QualificationSession.Checks) == 0 || len(result.QualificationSession.Checks) > 1024 {
			return result, fmt.Errorf("invalid qualification session response")
		}
		return result, nil
	}
	if kind == "notification-worker" {
		fmt.Fprintln(c.Err, "Qualification pauses an eligible active gateway until fresh observations are recorded and you apply again. Queued work and delivery ownership remain retained. Previously disabled commands stay disabled.")
	}
	result, err := call()
	if err != nil {
		return err
	}
	if !result.QualificationSession.Recorded {
		session := *result.QualificationSession
		fmt.Fprintln(c.Err, "Record observations you have actually performed. Use pending to defer a check; passed/failed requires an evidence file. No tests or provider sends run here.")
		if session.LEDEnvironmentRequired && !session.AcknowledgementPending {
			if kind != "notification-worker" {
				return fmt.Errorf("backup qualification returned unexpected LED environment requirements")
			}
			version, e := prompt.Ask(ctx, "Observed LED daemon version (leave blank to defer)", false)
			if e != nil {
				return e
			}
			version = strings.TrimSpace(version)
			if version != "" {
				file, e := prompt.Ask(ctx, "Operator-local copy of the LED daemon configuration used for these checks", false)
				if e != nil {
					return e
				}
				digest, e := qualificationEvidenceDigest(file)
				if e != nil {
					return e
				}
				r.Action = "qualify-observe"
				r.Observation = &install.QualificationObservation{Session: session.ID, LEDVersion: version, LEDConfigHash: digest}
				result, e = call()
				if e != nil {
					return e
				}
				if result.QualificationSession.ID != session.ID || result.QualificationSession.LEDEnvironmentRequired {
					return fmt.Errorf("LED environment recording is unconfirmed; rerun qualify to inspect the retained draft")
				}
			}
		}
		for _, check := range session.Checks {
			if session.AcknowledgementPending || check.Result != "pending" {
				continue
			}
			answer, err := prompt.Ask(ctx, check.Scope+" / "+check.ID+" (pending, passed, failed)", false)
			if err != nil {
				return err
			}
			answer = strings.ToLower(strings.TrimSpace(answer))
			if answer == "" || answer == "pending" {
				continue
			}
			if answer != "passed" && answer != "failed" {
				return fmt.Errorf("choose pending, passed or failed; prior observations remain in the draft")
			}
			file, err := prompt.Ask(ctx, "Operator-local evidence file for "+check.ID, false)
			if err != nil {
				return err
			}
			digest, err := qualificationEvidenceDigest(file)
			if err != nil {
				return err
			}
			r.Action = "qualify-observe"
			r.Observation = &install.QualificationObservation{Session: session.ID, Scope: check.Scope, ID: check.ID, Result: answer, EvidenceHash: digest}
			result, err = call()
			if err != nil {
				return err
			}
			if result.QualificationSession.ID != session.ID {
				return fmt.Errorf("qualification session changed during observations")
			}
		}
		r.Action, r.Observation = "qualify-commit", &install.QualificationObservation{Session: session.ID}
		result, err = call()
		if err != nil {
			return err
		}
		if result.QualificationSession.ID != session.ID || !result.QualificationSession.Recorded {
			return fmt.Errorf("qualification recording is unconfirmed; rerun qualify to inspect retained observations")
		}
	}
	if v.Options.JSON {
		return json.NewEncoder(c.Out).Encode(result)
	}
	_, err = fmt.Fprintf(c.Out, "%s: %s %s\n  Next: %s\n", v.Server, kind, result.State, result.Next)
	return err
}

func qualificationEvidenceDigest(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("operator home unavailable")
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	if path == "" {
		return "", fmt.Errorf("evidence file required for a completed observation")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("evidence file unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 16<<20 {
		return "", fmt.Errorf("evidence must be a nonempty regular file of at most 16 MiB")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (16<<20)+1))
	after, statErr := f.Stat()
	if err != nil || statErr != nil || n != info.Size() || n != after.Size() || !after.ModTime().Equal(info.ModTime()) {
		return "", fmt.Errorf("evidence file changed while reading")
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
