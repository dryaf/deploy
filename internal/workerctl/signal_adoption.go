package workerctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
)

// This records coordinator ownership of an already owned, linked native
// service. It creates no account, link, inbox or service receipt.
type signalAdoptionRecord struct {
	Journal  OperationJournal                       `json:"journal"`
	Inputs   signalSetupPins                        `json:"inputs"`
	Manifest install.FreshSignalManifest            `json:"manifest"`
	Account  signalaccount.NativeAccountReservation `json:"account"`
}

func (v signalAdoptionRecord) setup() signalSetupRecord {
	return signalSetupRecord{Version: 2, Account: v.Account, Spec: v.Manifest.Spec}
}

func validateSignalAdoption(v notificationAdoptionIntent) error {
	if v.Signal == nil || v.Manifest.FreshSignal == nil {
		return fmt.Errorf("native adoption receipt missing")
	}
	a := v.Signal
	m, s, j, p := a.Manifest, a.Manifest.Spec, a.Journal, a.Inputs
	hash := m.ManifestHash
	m.ManifestHash = ""
	if !validDigest(hash) || protocol.Hash(m) != hash || v.Manifest.FreshSignal.Root != s.Root || v.Manifest.FreshSignal.ManifestHash != hash || validateOperationJournal(j) != nil || !j.Complete || len(j.Children) != 1 || j.Children[0].Name != "signal-service" || !j.Children[0].Complete || j.Children[0].Operation != s.Operation || j.Children[0].Receipt != protocol.Hash(a.Manifest) {
		return fmt.Errorf("native adoption service authority changed")
	}
	want := OperationPins{DesiredScope: v.Journal.Pins.DesiredScope, Worker: v.Journal.Pins.Worker, Action: "apply", DesiredHash: v.Journal.Pins.DesiredHash, UID: v.Journal.Pins.UID, Home: v.Journal.Pins.Home, Installations: map[string]string{"signal-service": s.Root, "signal-account": s.AccountState, "signal-inbox": s.Inbox.State}, Artifacts: map[string]string{"helper": s.BinarySHA256, "signal": s.CLISHA256}}
	if !reflect.DeepEqual(j.Pins, want) || j.Pins.Artifacts["helper"] != v.Journal.Pins.Artifacts["helper"] || j.Pins.Artifacts["signal"] != v.Journal.Pins.Artifacts["signal"] || p.Version != 1 || p.Operation != s.Operation || p.DesiredHash != j.Pins.DesiredHash || p.HelperHash != s.BinarySHA256 || p.CLIHash != s.CLISHA256 || p.AccountHash != s.AccountSHA256 || p.RecipientHash != s.RecipientSHA256 || p.AccountHash != v.Inputs.SignalAccount || p.RecipientHash != v.Inputs.SignalRecipient {
		return fmt.Errorf("native adoption input pins changed")
	}
	socket := s.Socket
	if socket == "" {
		socket = filepath.Join(s.Root, "api.sock")
	}
	if p.Paths.Account != s.AccountState || p.Paths.Inbox != s.Inbox.State || p.Paths.Service != s.Root || p.Paths.Socket != socket || p.Paths.Executable != s.CLIFile || p.Paths.AccountFile != s.AccountFile || p.Paths.RecipientFile != s.RecipientFile || a.Account.Path != s.AccountState || s.UnitDirectory != v.Manifest.UnitDirectory || s.Version != 1 || s.CLIVersion != "0.14.9" || !safefs.ValidID(s.LinkOperation, "n") || !validDigest(s.LinkHash) {
		return fmt.Errorf("native adoption paths or link ownership changed")
	}
	return nil
}

func newSignalAdoption(r ControlRequest, paths ResolvedPaths, review install.PolicyReview) (*signalAdoptionRecord, error) {
	if review.Native == nil {
		return nil, fmt.Errorf("canonical native service review required")
	}
	m := *review.Native
	resolved, err := ResolveSignalPaths(r.Desired.Notification.Providers["signal"], paths, r.Facts.Home, m.Spec.CLISHA256)
	if err != nil {
		return nil, err
	}
	d, err := safefs.Open(m.Spec.AccountState)
	if err != nil {
		return nil, err
	}
	var account signalaccount.NativeAccountReservation
	err = d.ReadJSON("deploy-native-bootstrap.json", &account)
	d.Close()
	if err != nil {
		return nil, err
	}
	if err = signalaccount.InspectNativeAccountReservation(account); err != nil {
		return nil, err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return nil, err
	}
	pins := OperationPins{DesiredScope: r.DesiredScope, Worker: r.Worker, Action: "apply", DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Installations: map[string]string{"signal-service": resolved.Service, "signal-account": resolved.Account, "signal-inbox": resolved.Inbox}, Artifacts: map[string]string{"helper": m.Spec.BinarySHA256, "signal": m.Spec.CLISHA256}}
	return &signalAdoptionRecord{Journal: OperationJournal{Version: 1, ID: id, Pins: pins, Complete: true, Children: []ChildReference{{Name: "signal-service", Operation: m.Spec.Operation, Receipt: protocol.Hash(m), Complete: true}}}, Inputs: signalSetupPins{Version: 1, Operation: m.Spec.Operation, DesiredHash: r.DesiredHash, Paths: resolved, HelperHash: m.Spec.BinarySHA256, CLIHash: m.Spec.CLISHA256, AccountHash: m.Spec.AccountSHA256, RecipientHash: m.Spec.RecipientSHA256}, Manifest: m, Account: account}, nil
}

func stageSignalAdoption(stage *safefs.Directory, v notificationAdoptionIntent) error {
	if v.Signal == nil {
		return nil
	}
	if err := stage.Mkdir("signal"); err != nil {
		return err
	}
	d, err := stage.Sub("signal")
	if err != nil {
		return err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", true, true)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = retainAdoptionStageRecord(d, "inputs.json", v.Signal.Inputs); err != nil {
		return err
	}
	if err = retainAdoptionStageRecord(d, "service.json", v.Signal.setup()); err != nil {
		return err
	}
	return retainAdoptionStageRecord(d, "operation.json", v.Signal.Journal)
}

func verifySignalAdoptionFiles(d *safefs.Directory, v notificationAdoptionIntent) error {
	if v.Signal == nil {
		return nil
	}
	if v.Signal.Inputs.Paths.Control != filepath.Join(d.File().Name(), "signal") {
		return fmt.Errorf("adopted native child coordinator moved")
	}
	child, err := d.Sub("signal")
	if err != nil {
		return err
	}
	defer child.Close()
	// These adopted child records are immutable. A native resume caller may
	// already hold the child lease exclusively, so verify its existing lease
	// file without attempting a second flock on the same process's authority.
	lock, err := child.Open("operation.lock", os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer lock.Close()
	var j OperationJournal
	var inputs signalSetupPins
	var record signalSetupRecord
	if err = child.ReadJSON("operation.json", &j); err != nil {
		return err
	}
	if err = child.ReadJSON("inputs.json", &inputs); err != nil {
		return err
	}
	if err = child.ReadJSON("service.json", &record); err != nil {
		return err
	}
	if !reflect.DeepEqual(j, v.Signal.Journal) || !reflect.DeepEqual(inputs, v.Signal.Inputs) || !reflect.DeepEqual(record, v.Signal.setup()) {
		return fmt.Errorf("adopted native child evidence changed")
	}
	return signalaccount.InspectNativeAccountReservation(record.Account)
}

func verifyRetainedSignalAdoption(d *safefs.Directory) error {
	var v notificationAdoptionIntent
	if err := d.ReadJSON("adoption.json", &v); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := validateAdoptionIntent(v); err != nil {
		return err
	}
	return verifySignalAdoptionFiles(d, v)
}

func (s *signalSetupStep) verifyAdoptedRecord(record signalSetupRecord, op string) error {
	d, err := safefs.Open(filepath.Dir(s.paths.Control))
	if err != nil {
		return err
	}
	defer d.Close()
	var v notificationAdoptionIntent
	if err = d.ReadJSON("adoption.json", &v); err != nil {
		return err
	}
	if err = validateAdoptionIntent(v); err != nil {
		return err
	}
	if v.Signal == nil || !reflect.DeepEqual(record, v.Signal.setup()) || !reflect.DeepEqual(s.inputPins(op), v.Signal.Inputs) || s.paths.Control != filepath.Join(d.File().Name(), "signal") {
		return fmt.Errorf("adopted Signal setup differs from immutable ownership review")
	}
	return nil
}

func compileSignalAdoption(r ControlRequest, paths ResolvedPaths, review install.PolicyReview, secrets map[string][]byte) (install.RuntimeInput, error) {
	p := r.Desired.Notification.Providers["signal"]
	m := review.Native
	if m == nil || review.Signal == nil || !reflect.DeepEqual(m.Spec, *review.Signal) || m.Spec.BinarySHA256 != r.HelperSHA256 || p.AccountFile == "" || p.RecipientFile == "" || p.AccountInput != "" || p.RecipientInput != "" || p.Executable == "" || p.ExecutableSHA256 != m.Spec.CLISHA256 || r.SignalSHA256 != "" && r.SignalSHA256 != m.Spec.CLISHA256 {
		return install.RuntimeInput{}, fmt.Errorf("native adoption requires the owned service, matching helper and explicit target-file/executable references")
	}
	for field, path := range map[string]string{"signal_account": m.Spec.AccountFile, "signal_recipient": m.Spec.RecipientFile} {
		raw, err := ReadCredentialInput(path)
		if err != nil {
			return install.RuntimeInput{}, err
		}
		secrets[field] = raw
	}
	return CompileFreshSignalNotification(r.Desired, paths, r.Facts.Home, secrets, *m)
}
