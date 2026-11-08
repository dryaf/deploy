package workerctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

var errSignalLinkRequired = errors.New("signal_link_required: resume in a terminal to link the declared Signal account")

type signalLinkReviewRequired struct{ Operation string }

func (signalLinkReviewRequired) Error() string {
	return "Signal linking outcome requires explicit review; the retained attempt will not be repeated"
}

const signalLinkReviewReason = "operator reviewed linked devices and authorized inspection of the retained native account"

type signalSetupLifecycle struct {
	resume  func(context.Context, install.FreshSignalReference) (install.FreshSignalManifest, error)
	ensure  func(context.Context, install.FreshSignalSpec) (install.FreshSignalManifest, error)
	inspect func(context.Context, install.FreshSignalSpec) (install.FreshSignalManifest, bool, error)
	gateway func(context.Context, string) (install.InstallationObservation, error)
}

func defaultSignalSetupLifecycle() signalSetupLifecycle {
	return signalSetupLifecycle{resume: install.ResumeFreshSignal, ensure: install.EnsureFreshSignal, inspect: func(ctx context.Context, s install.FreshSignalSpec) (install.FreshSignalManifest, bool, error) {
		return install.InspectFreshSignalSetup(ctx, s, true)
	}, gateway: install.InspectInstallation}
}

type signalSetupPins struct {
	Version       int                 `json:"version"`
	Operation     string              `json:"operation"`
	DesiredHash   string              `json:"desired_hash"`
	Paths         ResolvedSignalPaths `json:"paths"`
	HelperHash    string              `json:"helper_hash"`
	CLIHash       string              `json:"cli_hash"`
	AccountHash   string              `json:"account_hash"`
	RecipientHash string              `json:"recipient_hash"`
}
type signalSetupRecord struct {
	Version int                                    `json:"version"`
	Account signalaccount.NativeAccountReservation `json:"account"`
	Spec    install.FreshSignalSpec                `json:"spec"`
}
type signalSetupStep struct {
	request                    ControlRequest
	gateway                    ResolvedPaths
	paths                      ResolvedSignalPaths
	directory                  *safefs.Directory
	provider                   config.ProviderDeclaration
	helperSource, nativeSource string
	account, recipient         []byte
	display                    io.Writer
	lifecycle                  signalSetupLifecycle
	manifest                   install.FreshSignalManifest
}

func (s *signalSetupStep) Name() string { return "signal-service" }
func (s *signalSetupStep) inputPins(op string) signalSetupPins {
	return signalSetupPins{Version: 1, Operation: op, DesiredHash: s.request.DesiredHash, Paths: s.paths, HelperHash: s.request.HelperSHA256, CLIHash: s.request.SignalSHA256, AccountHash: inputDigest(s.account), RecipientHash: inputDigest(s.recipient)}
}
func (s *signalSetupStep) pin(op string, create bool) (bool, error) {
	var actual signalSetupPins
	err := s.directory.ReadJSON("inputs.json", &actual)
	if errors.Is(err, os.ErrNotExist) {
		for _, path := range []string{s.paths.Account, s.paths.Inbox, s.paths.Service} {
			if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
				return false, fmt.Errorf("Signal input authority missing from existing setup")
			}
		}
		if !create {
			return false, nil
		}
		if err = s.directory.WriteJSON("inputs.json", s.inputPins(op), false); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if protocol.Hash(actual) != protocol.Hash(s.inputPins(op)) {
		return false, fmt.Errorf("Signal input or installation pins changed; review retained setup")
	}
	return true, nil
}
func (s *signalSetupStep) helperPath() string {
	return filepath.Join(s.gateway.Data, "signal", "artifacts", s.request.HelperSHA256, "deploy")
}
func (s *signalSetupStep) spec(op string, inbox relay.FreshInboxBootstrap, link signalaccount.Status) install.FreshSignalSpec {
	return install.FreshSignalSpec{Version: 1, Operation: op, Root: s.paths.Service, Socket: s.paths.Socket, UnitDirectory: s.gateway.UnitDirectory, BinaryFile: s.helperPath(), BinarySHA256: s.request.HelperSHA256, CLIFile: s.paths.Executable, CLISHA256: s.request.SignalSHA256, CLIVersion: "0.14.9", AccountState: s.paths.Account, AccountFile: s.paths.AccountFile, AccountSHA256: inputDigest(s.account), RecipientFile: s.paths.RecipientFile, RecipientSHA256: inputDigest(s.recipient), LinkOperation: link.Operation, LinkHash: link.RecordHash, Inbox: inbox}
}
func (s *signalSetupStep) record(op string) (signalSetupRecord, error) {
	var r signalSetupRecord
	if err := s.directory.ReadJSON("service.json", &r); err != nil {
		return r, err
	}
	if r.Version == 2 {
		if err := s.verifyAdoptedRecord(r, op); err != nil {
			return r, err
		}
	} else {
		expected := s.spec(op, r.Spec.Inbox, signalaccount.Status{Operation: op, RecordHash: r.Spec.LinkHash})
		if r.Version != 1 || r.Account.Path != s.paths.Account || r.Account.Operation != op || r.Spec.Inbox.Operation != op || protocol.Hash(r.Spec) != protocol.Hash(expected) || s.provider.Account != "" && r.Spec.Inbox.Account != s.provider.Account {
			return r, fmt.Errorf("Signal setup record changed")
		}
	}
	if err := signalaccount.InspectNativeAccountReservation(r.Account); err != nil {
		return r, fmt.Errorf("Signal account reservation unavailable: %v", err)
	}
	for path, want := range map[string][]byte{s.paths.AccountFile: s.account, s.paths.RecipientFile: s.recipient} {
		got, err := ReadCredentialInput(path)
		if err != nil || !bytes.Equal(got, want) {
			return r, fmt.Errorf("Signal private inputs changed")
		}
	}
	return r, nil
}
func (s *signalSetupStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	exists, err := s.pin(op, false)
	if err != nil || !exists {
		return "", false, err
	}
	r, err := s.record(op)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	m, complete, err := s.lifecycle.inspect(ctx, r.Spec)
	if err != nil || !complete {
		return "", false, err
	}
	if protocol.Hash(m.Spec) != protocol.Hash(r.Spec) {
		return "", false, fmt.Errorf("Signal service receipt differs from setup")
	}
	s.manifest = m
	return protocol.Hash(m), true, nil
}
func (s *signalSetupStep) Run(ctx context.Context, op string) error {
	for _, root := range []string{s.paths.Account, s.paths.Inbox, s.paths.Service, s.paths.Control, s.gateway.State, s.gateway.Installation, s.gateway.UnitDirectory} {
		if within(s.helperPath(), root) {
			return fmt.Errorf("Signal helper overlaps managed account or state")
		}
	}

	if _, err := s.pin(op, true); err != nil {
		return err
	}
	r, err := s.record(op)
	if err == nil {
		_, err = s.lifecycle.ensure(ctx, r.Spec)
		return err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, path := range []string{filepath.Dir(s.paths.Account), filepath.Dir(s.paths.Inbox), s.gateway.UnitDirectory} {
		d, err := ensureConfigDirectory(path)
		if err != nil {
			return err
		}
		d.Close()
	}
	if err = publishSignalExecutable(s.helperPath(), s.helperSource, s.request.HelperSHA256, s.request.Facts.Arch, true); err != nil {
		return err
	}
	if s.provider.Executable == "" {
		if err = publishSignalExecutable(s.paths.Executable, s.nativeSource, s.request.SignalSHA256, s.request.Facts.Arch, false); err != nil {
			return err
		}
	} else if err = VerifySignalArtifact(Artifact{s.paths.Executable, s.request.SignalSHA256}, s.request.Facts.Arch); err != nil {
		return err
	}
	if s.provider.AccountInput != "" {
		if err = publishPrivateInput(s.paths.AccountFile, s.account); err != nil {
			return err
		}
	}
	if s.provider.RecipientInput != "" {
		if err = publishPrivateInput(s.paths.RecipientFile, s.recipient); err != nil {
			return err
		}
	}
	reserved, err := signalaccount.BootstrapNativeAccount(ctx, s.paths.Account, op)
	if err != nil {
		return err
	}
	linked, err := signalaccount.Inspect(s.paths.Account)
	if err != nil {
		return err
	}
	if linked.Operation != "" && linked.Operation != op {
		return fmt.Errorf("Signal account has another link operation; explicit adoption required")
	}
	if s.request.SignalReview != "" {
		if s.request.SignalReview != op || linked.Operation != op || (linked.State != "uncertain" && linked.State != "reviewed") {
			return fmt.Errorf("Signal review does not match the retained uncertain attempt")
		}
		linked, err = signalaccount.Resolve(s.paths.Account, op, signalLinkReviewReason)
		if err != nil {
			return err
		}
	}
	if linked.State == "uncertain" {
		return signalLinkReviewRequired{Operation: op}
	}
	native := signalaccount.NativeCLI{Path: s.paths.Executable, SHA256: s.request.SignalSHA256, Version: "0.14.9"}
	output := s.display
	if output == nil {
		if linked.State != "linked" && linked.State != "reviewed" {
			return errSignalLinkRequired
		}
		output = io.Discard
	}
	linked, err = signalaccount.LinkNative(ctx, s.paths.Account, "deploy-notification-worker", op, native, output)
	if err != nil {
		if linked.State == "uncertain" && linked.Operation == op {
			return signalLinkReviewRequired{Operation: op}
		}
		return err
	}
	if !linked.RuntimeAllowed || linked.Operation != op {
		return fmt.Errorf("Signal linking outcome requires explicit account review")
	}
	account, err := signalaccount.InspectNativeAccount(ctx, s.paths.Account, s.paths.AccountFile, native, linked.Operation, linked.RecordHash)
	if err != nil {
		return err
	}
	if s.provider.Account != "" && s.provider.Account != account {
		return fmt.Errorf("linked Signal account differs from declaration")
	}
	inbox, err := relay.BootstrapFreshInbox(ctx, s.paths.Inbox, op, account)
	if err != nil {
		return err
	}
	serviceRoot, err := ensureConfigDirectory(s.paths.Service)
	if err != nil {
		return err
	}
	serviceRoot.Close()
	r = signalSetupRecord{Version: 1, Account: reserved, Spec: s.spec(op, inbox, linked)}
	if err = s.directory.WriteJSON("service.json", r, false); err != nil {
		return err
	}
	_, err = s.lifecycle.ensure(ctx, r.Spec)
	return err
}

func publishSignalExecutable(path, source, hash, arch string, helper bool) error {
	verify := func(a Artifact) error {
		if helper {
			return VerifyArtifact(a, arch, true)
		}
		return VerifySignalArtifact(a, arch)
	}
	if !validDigest(hash) {
		return fmt.Errorf("Signal artifact pin required")
	}
	d, err := ensureConfigDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	f, err := d.Open(filepath.Base(path), unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err == nil {
		info, e := f.Stat()
		f.Close()
		if e != nil {
			return e
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || info.Mode().Perm() != 0700 {
			return fmt.Errorf("staged Signal executable ownership changed")
		}
		return verify(Artifact{path, hash})
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = verify(Artifact{source, hash}); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("Signal artifact source unavailable")
	}
	defer input.Close()
	raw, err := io.ReadAll(io.LimitReader(input, (512<<20)+1))
	if err != nil || len(raw) > 512<<20 || inputDigest(raw) != hash {
		return fmt.Errorf("Signal artifact changed during staging")
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return err
	}
	temp := "." + id
	f, err = d.Open(temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0700)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(d.File().Fd()), temp, 0)
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return d.Rename(temp, d, filepath.Base(path))
}

// prepareDeclaredSignal owns a separate child coordinator. The gateway's
// single bootstrap/enrollment receipt stays unchanged in shape, and its managed
// manifest retains the exact Signal child reference.
func prepareDeclaredSignal(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper, nativeSource string, display io.Writer, lifecycle signalSetupLifecycle) (install.FreshSignalManifest, ResolvedSignalPaths, map[string][]byte, error) {
	var zero install.FreshSignalManifest
	var resolved ResolvedSignalPaths
	if err := directNotificationSupport(r.Desired); err != nil {
		return zero, resolved, nil, err
	}
	if r.Desired.Notification == nil {
		return zero, resolved, nil, fmt.Errorf("Signal declaration missing")
	}
	p, ok := r.Desired.Notification.Providers["signal"]
	if !ok {
		return zero, resolved, nil, fmt.Errorf("Signal provider missing")
	}
	if p.Executable != "" {
		if r.SignalSHA256 != "" && r.SignalSHA256 != p.ExecutableSHA256 {
			return zero, resolved, nil, fmt.Errorf("Signal artifact differs from YAML override")
		}
		r.SignalSHA256 = p.ExecutableSHA256
	}
	var err error
	resolved, err = ResolveSignalPaths(p, paths, r.Facts.Home, r.SignalSHA256)
	if err != nil {
		return zero, resolved, nil, err
	}
	secrets := map[string][]byte{}
	for _, input := range []struct{ field, local, target string }{{"signal_account", p.AccountInput, resolved.AccountFile}, {"signal_recipient", p.RecipientInput, resolved.RecipientFile}} {
		raw, supplied := r.Inputs[input.field]
		if input.local != "" {
			if !supplied {
				return zero, resolved, nil, fmt.Errorf("missing private Signal input")
			}
		} else {
			if supplied {
				return zero, resolved, nil, fmt.Errorf("unexpected private Signal input")
			}
			raw, err = ReadCredentialInput(input.target)
			if err != nil {
				return zero, resolved, nil, err
			}
		}
		if !validSignalPrivateInput(raw) {
			return zero, resolved, nil, fmt.Errorf("invalid private Signal input")
		}
		secrets[input.field] = raw
	}
	if err = readDeclaredTelegramInputs(r, paths, secrets); err != nil {
		return zero, resolved, nil, err
	}
	for field := range r.Inputs {
		if _, ok := secrets[field]; !ok {
			return zero, resolved, nil, fmt.Errorf("unexpected credential field")
		}
	}
	// Validate the entire provider/route declaration before account linking or
	// permanent native service ownership, including combined Telegram inputs.
	compiled, err := compileDirectSignalNotification(r.Desired, paths, r.Facts.Home, secrets, resolved)
	if err != nil {
		return zero, resolved, nil, err
	}
	if _, err = compiled.Resolve(); err != nil {
		return zero, resolved, nil, err
	}
	// Refuse a changed/removed gateway before linking or provisioning its child.
	var previous OperationJournal
	var parent *safefs.Directory
	if d, e := safefs.Open(paths.Control); e == nil {
		defer d.Close()
		e = d.ReadJSON("operation.json", &previous)
		if e == nil {
			lock, err := d.Lock("operation.lock", false, false)
			if err != nil {
				return zero, resolved, nil, err
			}
			defer lock.Close()
			if err = d.ReadJSON("operation.json", &previous); err != nil {
				return zero, resolved, nil, err
			}
			parent = d
			if validateOperationJournal(previous) != nil || previous.Pins.Action != "apply" || previous.Pins.Worker != r.Worker || previous.Pins.DesiredHash != r.DesiredHash || previous.Pins.DesiredScope != r.DesiredScope || previous.Pins.UID != r.Facts.UID || previous.Pins.Home != r.Facts.Home || previous.Pins.Artifacts["helper"] != r.HelperSHA256 || previous.Pins.Installations["notification-worker"] != paths.Installation || previous.Pins.Installations["runtime"] != paths.Runtime || previous.Pins.Installations["state"] != paths.State {
				return zero, resolved, nil, fmt.Errorf("gateway policy or ownership changed; finish its lifecycle before Signal setup")
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return zero, resolved, nil, e
		} else {
			for _, path := range []string{paths.Runtime, paths.State, paths.Installation} {
				if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
					return zero, resolved, nil, fmt.Errorf("gateway coordinator authority missing")
				}
			}
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return zero, resolved, nil, e
	} else {
		for _, path := range []string{paths.Runtime, paths.State, paths.Installation} {
			if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
				return zero, resolved, nil, fmt.Errorf("gateway coordinator authority missing")
			}
		}
	}
	if !r.InteractiveLink {
		display = nil
	}
	open := ensureConfigDirectory
	if parent != nil {
		// A gateway can only exist after its native child was journaled. Do not
		// recreate missing child authority during a later gateway transition.
		open = safefs.Open
	}
	d, err := open(resolved.Control)
	if err != nil {
		return zero, resolved, nil, err
	}
	defer d.Close()
	step := &signalSetupStep{request: r, gateway: paths, paths: resolved, directory: d, provider: p, helperSource: helper, nativeSource: nativeSource, account: secrets["signal_account"], recipient: secrets["signal_recipient"], display: display, lifecycle: lifecycle}
	pins := OperationPins{DesiredScope: r.DesiredScope, Worker: r.Worker, Action: "apply", DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Artifacts: map[string]string{"helper": r.HelperSHA256, "signal": r.SignalSHA256}, Installations: map[string]string{"signal-service": resolved.Service, "signal-account": resolved.Account, "signal-inbox": resolved.Inbox}}
	if parent != nil && !previous.Complete && limitsJournal(previous) && policyJournalKind(previous) == "providers" {
		// Validate while only the gateway lock is held; the native verifier
		// acquires its own child lock and must precede the reuse lock below.
		if err = verifyPendingProviderSignal(ctx, r, paths, parent, previous, lifecycle); err != nil {
			return zero, resolved, nil, err
		}
	}
	if parent != nil && !previous.Complete && deliveryProviderJournal(previous) {
		if err = verifyPendingDeliveryProviderSignal(r, paths, parent, previous); err != nil {
			return zero, resolved, nil, err
		}
	}
	if parent != nil && !previous.Complete && deliveryOutputJournal(previous) {
		if err = verifyPendingDeliveryOutputSignal(r, paths, parent, previous); err != nil {
			return zero, resolved, nil, err
		}
	}
	if parent != nil && !previous.Complete && enrolledOutputJournal(previous) {
		if err = verifyPendingEnrolledOutputSignal(r, paths, parent, previous); err != nil {
			return zero, resolved, nil, err
		}
	}
	if parent != nil && !previous.Complete && sharedLimitsJournal(previous) {
		if err = verifyPendingSharedLimitsSignal(r, paths, parent, previous); err != nil {
			return zero, resolved, nil, err
		}
	}
	if parent != nil && !previous.Complete && remappingJournal(previous) {
		if err = verifyPendingMappingSignal(r, paths, parent, previous); err != nil {
			return zero, resolved, nil, err
		}
	}
	if err = step.resumeStoppedChild(ctx, pins); err != nil {
		return zero, resolved, nil, err
	}
	if reused, err := step.reuse(ctx, pins, parent, previous); reused || err != nil {
		return step.manifest, resolved, secrets, err
	}
	_, err = (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step})
	return step.manifest, resolved, secrets, err
}

// A completed native service keeps its own immutable setup policy and helper.
// Gateway-only transitions may reference it, but cannot rewrite those pins or
// relink the account. Lock order is gateway coordinator, Signal coordinator,
// then the domain inspection locks; neither coordinator is mutated here.
func (s *signalSetupStep) reuse(ctx context.Context, wanted OperationPins, parent *safefs.Directory, gateway OperationJournal) (bool, error) {
	var child OperationJournal
	err := s.directory.ReadJSON("operation.json", &child)
	if errors.Is(err, os.ErrNotExist) {
		if parent != nil {
			return false, fmt.Errorf("Signal coordinator authority missing from existing gateway")
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if reflect.DeepEqual(child.Pins, wanted) {
		return false, nil
	}
	lock, err := s.directory.Lock("operation.lock", s.request.Action == "resume", false)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err = s.directory.ReadJSON("operation.json", &child); err != nil {
		return false, err
	}
	if parent == nil || validateOperationJournal(child) != nil || !child.Complete || len(child.Children) != 1 || child.Children[0].Name != "signal-service" {
		return false, fmt.Errorf("finish retained gateway and Signal setup before changing their policy")
	}
	// Only gateway policy/scope and its helper may evolve independently.
	expected := wanted
	expected.DesiredHash, expected.DesiredScope = child.Pins.DesiredHash, child.Pins.DesiredScope
	expected.Artifacts = map[string]string{"helper": child.Pins.Artifacts["helper"], "signal": wanted.Artifacts["signal"]}
	if !reflect.DeepEqual(expected, child.Pins) {
		return false, fmt.Errorf("Signal service inputs or ownership changed; explicit service migration required")
	}
	original := *s
	original.request.DesiredHash, original.request.DesiredScope = child.Pins.DesiredHash, child.Pins.DesiredScope
	original.request.HelperSHA256 = child.Pins.Artifacts["helper"]
	var retained signalSetupPins
	if err = s.directory.ReadJSON("inputs.json", &retained); err != nil {
		return false, err
	}
	selectedPaths := s.paths
	selectedPaths.AccountFile, selectedPaths.RecipientFile = retained.Paths.AccountFile, retained.Paths.RecipientFile
	if selectedPaths != retained.Paths {
		return false, fmt.Errorf("native Signal service paths changed")
	}
	if s.paths != retained.Paths || inputDigest(s.account) != retained.AccountHash || inputDigest(s.recipient) != retained.RecipientHash {
		if err = install.InspectRetainedFreshSignalDeliverySelection(s.paths.Service, s.paths.AccountFile, s.paths.RecipientFile, inputDigest(s.account), inputDigest(s.recipient)); err != nil {
			return false, err
		}
		original.paths = retained.Paths
		original.account, err = ReadCredentialInput(retained.Paths.AccountFile)
		if err != nil {
			return false, err
		}
		original.recipient, err = ReadCredentialInput(retained.Paths.RecipientFile)
		if err != nil {
			return false, err
		}
	}
	providerPending := !gateway.Complete && limitsJournal(gateway) && policyJournalKind(gateway) == "providers"
	// The native child has its own unchanged receipt. Explicit resume repairs
	// it before gateway inspection, which may itself depend on that child's
	// completed restart. This does not complete or change any gateway operation.
	if err = original.resumeStoppedRecord(ctx, child.Children[0].Operation, child.Children[0].Receipt); err != nil {
		return false, err
	}
	if s.lifecycle.gateway == nil {
		return false, fmt.Errorf("canonical gateway inspection required for Signal reuse")
	}
	o, err := s.lifecycle.gateway(ctx, s.gateway.Installation)
	if err != nil {
		return false, err
	}
	receipt, valid := canonicalNotificationReceipt(gateway)
	if !valid || o.Manifest.Mode == "uninstalled" || o.Manifest.FreshSignal == nil {
		return false, fmt.Errorf("gateway authority differs from retained Signal reference")
	}
	if gateway.Complete {
		if o.Phase != "complete" || receipt != protocol.Hash(o.Manifest) {
			return false, fmt.Errorf("canonical gateway receipt changed")
		}
		if err = verifyActivityReceipt(parent, gateway, o.Manifest); err != nil {
			return false, err
		}
		if err = verifyReleaseReceipt(parent, gateway, o.Manifest); err != nil {
			return false, err
		}
	} else if !providerPending {
		// Enrollment/admission may have committed the domain operation before
		// its outer receipt. Reuse only the unchanged, independently complete
		// Signal child; the caller must still resume and verify that gateway
		// operation through its own lifecycle. This is not gateway completion.
		if len(gateway.Children) != 1 || (!enrolledNotificationJournal(gateway) && !deliveryOutputJournal(gateway) && gateway.Children[0].Name != "notification-delivery") {
			return false, fmt.Errorf("finish gateway transition before Signal reuse")
		}
		var prior OperationJournal
		if err = parent.ReadJSON("history/"+gateway.Pins.PreviousOperation+".json", &prior); err != nil {
			return false, err
		}
		if validateOperationJournal(prior) != nil || !prior.Complete || prior.ID != gateway.Pins.PreviousOperation || prior.Digest() != gateway.Pins.Previous || (!remappingJournal(gateway) && !enrolledOutputJournal(gateway) && !deliveryOutputJournal(gateway) && !sharedLimitsJournal(gateway) && !deliveryProviderJournal(gateway) && prior.Pins.DesiredHash != gateway.Pins.DesiredHash) || prior.Pins.DesiredScope != gateway.Pins.DesiredScope || prior.Pins.Action != "apply" || prior.Pins.Worker != gateway.Pins.Worker || prior.Pins.UID != gateway.Pins.UID || prior.Pins.Home != gateway.Pins.Home || !reflect.DeepEqual(prior.Pins.Installations, gateway.Pins.Installations) || prior.Pins.Artifacts["helper"] != gateway.Pins.Artifacts["helper"] {
			return false, fmt.Errorf("pending gateway predecessor changed")
		}
		if gateway.Children[0].Name == "notification-enrollment" && !unboundPredecessor(prior) || !remappingJournal(gateway) && mappingJournal(gateway) && (len(prior.Children) != 1 || prior.Children[0].Name != "notification-enrollment") || gateway.Children[0].Name == "notification-delivery" && !enrolledNotificationJournal(prior) {
			return false, fmt.Errorf("pending gateway predecessor has a different lifecycle")
		}
	}
	receipt, complete, err := original.Inspect(ctx, child.Children[0].Operation)
	if err != nil {
		return false, err
	}
	if !complete || receipt != child.Children[0].Receipt || o.Manifest.FreshSignal.Root != original.manifest.Spec.Root || o.Manifest.FreshSignal.ManifestHash != original.manifest.ManifestHash {
		return false, fmt.Errorf("retained Signal service receipt missing or changed")
	}
	s.manifest = original.manifest
	return true, nil
}
func validSignalPrivateInput(raw []byte) bool {
	value := strings.TrimSpace(string(raw))
	return len(raw) > 0 && len(raw) <= 4096 && value != "" && !strings.ContainsAny(value, "\x00\r\n")
}

// Explicit short resume may restart the intact native service. Inspect remains
// read-only; both coordinator receipts and the account link remain unchanged.
func (s *signalSetupStep) resumeStoppedChild(ctx context.Context, wanted OperationPins) error {
	if s.request.Action != "resume" || s.lifecycle.resume == nil {
		return nil
	}
	var child OperationJournal
	if err := s.directory.ReadJSON("operation.json", &child); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !reflect.DeepEqual(child.Pins, wanted) {
		return nil
	}
	lock, err := s.directory.Lock("operation.lock", true, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = s.directory.ReadJSON("operation.json", &child); err != nil {
		return err
	}
	if validateOperationJournal(child) != nil || !reflect.DeepEqual(child.Pins, wanted) {
		return fmt.Errorf("Signal coordinator changed before resume")
	}
	if !child.Complete {
		return nil
	}
	if len(child.Children) != 1 || child.Children[0].Name != "signal-service" {
		return fmt.Errorf("Signal resume requires retained service receipt")
	}
	return s.resumeStoppedRecord(ctx, child.Children[0].Operation, child.Children[0].Receipt)
}
func (s *signalSetupStep) resumeStoppedRecord(ctx context.Context, operation, receipt string) error {
	if s.request.Action != "resume" || s.lifecycle.resume == nil {
		return nil
	}
	exists, err := s.pin(operation, false)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("Signal resume inputs missing")
	}
	record, err := s.record(operation)
	if err != nil {
		return err
	}
	m, complete, err := s.lifecycle.inspect(ctx, record.Spec)
	if err == nil {
		return nil
	}
	var needed install.FreshSignalResumeRequired
	if !errors.As(err, &needed) {
		return err
	}
	if complete || protocol.Hash(m) != receipt || protocol.Hash(m.Spec) != protocol.Hash(record.Spec) || needed.Reference.Root != m.Spec.Root || needed.Reference.ManifestHash != m.ManifestHash {
		return fmt.Errorf("Signal resume authority changed")
	}
	got, err := s.lifecycle.resume(ctx, needed.Reference)
	if err != nil {
		return signalServiceResumeError{Server: s.request.Server, Cause: err}
	}
	if protocol.Hash(got) != receipt {
		return fmt.Errorf("Signal resume changed service ownership")
	}
	return nil
}

// Completed gateway transitions can return before the ordinary child setup
// path. Handle explicit resumption of an already-completed Signal child first;
// absent or unfinished children still follow the normal linking/setup flow.
func resumeCompletedSignalChild(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper, native string, lifecycle signalSetupLifecycle) error {
	if r.Action != "resume" || r.Desired.Notification == nil || !config.WorkerEnabled(r.Desired.Notification.Enabled) {
		return nil
	}
	if _, ok := r.Desired.Notification.Providers["signal"]; !ok {
		return nil
	}
	if parent, e := safefs.Open(paths.Control); e == nil {
		var gateway OperationJournal
		e = parent.ReadJSON("operation.json", &gateway)
		parent.Close()
		if e == nil && adoptionJournal(gateway) && !gateway.Complete {
			// Pending adoption verifies its original canonical domain first. It
			// does not grant service mutation before ownership publication ends.
			return nil
		}
	}
	d, err := safefs.Open(filepath.Join(paths.Control, "signal"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var j OperationJournal
	err = d.ReadJSON("operation.json", &j)
	d.Close()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if validateOperationJournal(j) != nil {
		return fmt.Errorf("Signal child journal changed")
	}
	if !j.Complete {
		return nil
	}
	_, _, _, err = prepareDeclaredSignal(ctx, r, paths, helper, native, nil, lifecycle)
	return err
}

type signalServiceResumeError struct {
	Server string
	Cause  error
}

func (e signalServiceResumeError) Error() string { return e.Cause.Error() }
func (e signalServiceResumeError) Unwrap() error { return e.Cause }
