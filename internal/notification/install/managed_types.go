package install

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

const managedUnit = "deploy-notification-worker.service"
const managedVersion = 1
const maxManagedBinary = 128 << 20

// ManagedSpec is an explicit local installation request. State must already be
// initialized by `init`; installation never initializes or replaces a database.
type ManagedSpec struct {
	FreshSignal         *FreshSignalReference                   `json:"fresh_signal,omitempty"`
	SignalConversations map[string]SignalConversationEnrollment `json:"signal_conversations,omitempty"`
	SignalVerification  *SignalVerificationReference            `json:"signal_verification,omitempty"`
	RecoveryCommitHash  string                                  `json:"recovery_commit_hash,omitempty"`
	Version             int                                     `json:"version"`
	Root                string                                  `json:"root"`
	State               string                                  `json:"state"`
	UnitDirectory       string                                  `json:"unit_directory"`
	RuntimeFile         string                                  `json:"runtime_file"`
	BinaryFile          string                                  `json:"binary_file"`
	BinarySHA256        string                                  `json:"binary_sha256"`
	DeniedPaths         []string                                `json:"denied_paths,omitempty"`
}

type ManagedReportCredentialHistory struct {
	Operation string `json:"operation"`
	Hash      string `json:"hash"`
}

type ManagedManifest struct {
	ReportCredentialHistory *ManagedReportCredentialHistory `json:"report_credential_history,omitempty"`
	LEDOutput               *ManagedLEDOutput               `json:"led_output,omitempty"`
	FreshSignal             *FreshSignalReference           `json:"fresh_signal,omitempty"`
	CommandHistoryHash      string                          `json:"command_history_hash,omitempty"`
	Commands                *ManagedCommandAdmission        `json:"commands,omitempty"`
	DeliveryRecovery        *worker.ManagedDeliveryRecovery `json:"delivery_recovery,omitempty"`
	DeliveryOwnerConfigHash string                          `json:"delivery_owner_config_hash,omitempty"`
	DeliveryOwnerHash       string                          `json:"delivery_owner_hash,omitempty"`
	DeliveryActivation      *ManagedDeliveryActivation      `json:"delivery_activation,omitempty"`
	SignalProfiles          *ManagedSignalProfiles          `json:"signal_profiles,omitempty"`
	SignalVerification      *ManagedSignalBinding           `json:"signal_verification,omitempty"`
	TelegramRenewalID       string                          `json:"telegram_renewal_id,omitempty"`
	RecoveryCommitHash      string                          `json:"recovery_commit_hash,omitempty"`
	Version                 int                             `json:"version"`
	Installation            string                          `json:"installation"`
	Revision                string                          `json:"revision"`
	Root                    string                          `json:"root"`
	UnitDirectory           string                          `json:"unit_directory"`
	Paths                   ServicePaths                    `json:"paths"`
	Identity                protocol.Identity               `json:"identity"`
	RuntimeHash             string                          `json:"runtime_hash"`
	ConfigHash              string                          `json:"config_hash"`
	BinaryHash              string                          `json:"binary_hash"`
	HelperRevision          string                          `json:"helper_revision,omitempty"`
	HelperHash              string                          `json:"helper_hash"`
	UnitHash                string                          `json:"unit_hash"`
	Schema                  int                             `json:"schema"`
	Mode                    string                          `json:"mode"` // verification or uninstalled; never production authority.
}

type managedIntent struct {
	DeliveryProvider *deliveryProviderTransition `json:"delivery_provider,omitempty"`
	Version          int
	ID               string
	Operation        string
	Before           *ManagedManifest
	After            ManagedManifest
	BeforeUnit       string
	AfterUnit        string
	Supersedes       *managedIntent `json:"supersedes,omitempty"` // One unfinished setup/update; never a recursive cancellation chain.
}

type managedTelegramRequest struct {
	Operation    string
	ManifestHash string
	Request      worker.TelegramRenewalRequest
}

type managedJournal struct {
	Intent managedIntent
	Phase  string
}

func managedDigest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func managedPath(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && p != "/" && !strings.ContainsAny(p, "\x00\r\n")
}
func managedWithin(path, root string) bool { return path == root || strings.HasPrefix(path, root+"/") }

func (s ManagedSpec) validate() error {
	if s.FreshSignal != nil {
		if !managedPath(s.FreshSignal.Root) || !safefs.DigestPattern.MatchString(s.FreshSignal.ManifestHash) || s.SignalVerification != nil || len(s.SignalConversations) != 0 || s.RecoveryCommitHash != "" {
			return fmt.Errorf("fresh Signal enrollment excludes recovered-account authority")
		}
	}

	if len(s.SignalConversations) > 32 {
		return fmt.Errorf("too many Signal conversation enrollments")
	}
	for name, binding := range s.SignalConversations {
		if !protocol.ValidToken(name) || !safefs.DigestPattern.MatchString(binding.PreviousDestination) || !protocol.ValidToken(binding.Conversation) {
			return fmt.Errorf("Signal conversation enrollment requires exact profile, old destination and conversation")
		}
	}
	if s.SignalVerification != nil {
		if err := s.SignalVerification.validate(); err != nil {
			return err
		}
	}
	if s.RecoveryCommitHash != "" && !safefs.DigestPattern.MatchString(s.RecoveryCommitHash) {
		return fmt.Errorf("invalid recovery publication commit")
	}
	if s.Version != managedVersion || !safefs.DigestPattern.MatchString(s.BinarySHA256) || len(s.DeniedPaths) > 64 {
		return fmt.Errorf("invalid managed installation spec")
	}
	for _, p := range []string{s.Root, s.State, s.UnitDirectory, s.RuntimeFile, s.BinaryFile} {
		if !managedPath(p) {
			return fmt.Errorf("managed installation paths must be clean absolute paths")
		}
	}
	for _, pair := range [][2]string{{s.Root, s.State}, {s.Root, s.UnitDirectory}, {s.State, s.UnitDirectory}} {
		if managedWithin(pair[0], pair[1]) || managedWithin(pair[1], pair[0]) {
			return fmt.Errorf("installation, state and unit directories must not overlap")
		}
	}
	if managedWithin(s.BinaryFile, s.Root) || managedWithin(s.RuntimeFile, s.Root) {
		return fmt.Errorf("stage installation inputs outside the managed root")
	}
	return nil
}

func privateManagedDir(path string, private bool) (*safefs.Directory, error) {
	if !managedPath(path) {
		return nil, fmt.Errorf("invalid managed directory")
	}
	d, err := safefs.Open(path)
	if err != nil {
		return nil, err
	}
	i, err := d.File().Stat()
	if err == nil {
		st, ok := i.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || i.Mode().Perm()&0022 != 0 || private && i.Mode().Perm() != 0700 {
			err = fmt.Errorf("unsafe managed directory ownership or permissions: %s (mode %04o)", path, i.Mode().Perm())
		}
	}
	if err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func managedHelperName(m ManagedManifest) string {
	if m.HelperRevision != "" {
		return "releases/" + m.HelperRevision + "/recovery-helper"
	}
	return "recovery-helper"
}

func (m ManagedManifest) validate() error {
	if m.LEDOutput != nil && !safefs.ValidID(m.LEDOutput.Operation, "n") {
		return fmt.Errorf("invalid managed LED output operation")
	}
	if m.FreshSignal != nil {
		if !managedPath(m.FreshSignal.Root) || !safefs.DigestPattern.MatchString(m.FreshSignal.ManifestHash) || m.SignalVerification != nil || m.SignalProfiles != nil || m.RecoveryCommitHash != "" {
			return fmt.Errorf("fresh Signal manifest carries incompatible ownership")
		}
		for _, root := range append([]string{m.Root, m.Paths.State, m.UnitDirectory}, m.Paths.DeniedPaths...) {
			if managedWithin(m.FreshSignal.Root, root) || managedWithin(root, m.FreshSignal.Root) {
				return fmt.Errorf("fresh Signal service overlaps gateway authority")
			}
		}
	}

	if h := m.ReportCredentialHistory; h != nil && (!safefs.ValidID(h.Operation, "n") || !safefs.DigestPattern.MatchString(h.Hash)) {
		return fmt.Errorf("invalid report credential history pin")
	}
	if m.CommandHistoryHash != "" && !safefs.DigestPattern.MatchString(m.CommandHistoryHash) {
		return fmt.Errorf("invalid command history pin")
	}
	if m.Commands != nil && m.CommandHistoryHash != m.Commands.Hash {
		return fmt.Errorf("command admission lost history pin")
	}
	if m.Commands != nil && (m.Mode != "delivery" || m.Commands.validate() != nil) {
		return fmt.Errorf("invalid managed command admission")
	}
	if m.DeliveryOwnerConfigHash != "" && (m.DeliveryOwnerHash == "" || !safefs.DigestPattern.MatchString(m.DeliveryOwnerConfigHash)) {
		return fmt.Errorf("invalid original delivery configuration pin")
	}
	if m.DeliveryOwnerHash != "" && !safefs.DigestPattern.MatchString(m.DeliveryOwnerHash) {
		return fmt.Errorf("invalid delivery ownership binding")
	}
	if m.DeliveryRecovery != nil {
		r := m.DeliveryRecovery
		if r.Validate() != nil || r.After != m.Identity || r.CommitHash != m.RecoveryCommitHash || protocol.Hash(r.Owner) != m.DeliveryOwnerHash || r.Owner.Root != m.Root || r.Owner.Installation != m.Installation {
			return fmt.Errorf("invalid recovered delivery ownership binding")
		}
	} else if m.DeliveryOwnerHash != "" && m.RecoveryCommitHash != "" {
		return fmt.Errorf("recovered delivery owner requires an explicit lineage")
	}
	if m.Mode == "delivery" {
		if m.DeliveryOwnerHash == "" || m.DeliveryActivation == nil || !safefs.ValidID(m.DeliveryActivation.Operation, "n") || !safefs.DigestPattern.MatchString(m.DeliveryActivation.Hash) {
			return fmt.Errorf("delivery mode has no activation authority")
		}
	} else if m.DeliveryActivation != nil {
		return fmt.Errorf("inactive mode carries delivery activation")
	}
	if m.SignalProfiles != nil && (m.SignalProfiles.validate() != nil || m.SignalVerification == nil || m.SignalVerification.ServiceRoot == "") {
		return fmt.Errorf("Signal profiles require a pinned supervised endpoint")
	}
	if m.SignalVerification != nil {
		if m.SignalVerification.validate() != nil || !safefs.ValidID(m.SignalVerification.Operation, "n") || m.RecoveryCommitHash == "" {
			return fmt.Errorf("invalid managed Signal verification binding")
		}
		for _, root := range []string{m.Root, m.Paths.State, m.UnitDirectory} {
			if managedWithin(m.SignalVerification.Root, root) || managedWithin(root, m.SignalVerification.Root) {
				return fmt.Errorf("Signal endpoint overlaps installation authority")
			}
		}
		for _, path := range m.Paths.DeniedPaths {
			if managedWithin(m.SignalVerification.Root, path) || managedWithin(path, m.SignalVerification.Root) {
				return fmt.Errorf("Signal verification endpoint is excluded")
			}
		}
	}

	if m.TelegramRenewalID != "" && (!safefs.ValidID(m.TelegramRenewalID, "n") || m.RecoveryCommitHash == "") {
		return fmt.Errorf("invalid managed Telegram renewal pin")
	}
	if m.RecoveryCommitHash != "" && !safefs.DigestPattern.MatchString(m.RecoveryCommitHash) {
		return fmt.Errorf("invalid recovery publication commit")
	}
	if m.HelperRevision != "" && !safefs.ValidID(m.HelperRevision, "n") {
		return fmt.Errorf("invalid recovery helper revision")
	}

	if m.Version != managedVersion || !safefs.ValidID(m.Installation, "n") || !safefs.ValidID(m.Revision, "n") || !m.Identity.Valid() || !managedPath(m.Root) || !managedPath(m.UnitDirectory) || m.Schema < 6 || (m.Mode != "verification" && m.Mode != "delivery" && m.Mode != "uninstalled") {
		return fmt.Errorf("invalid notification installation manifest")
	}
	for _, h := range []string{m.RuntimeHash, m.ConfigHash, m.BinaryHash, m.HelperHash, m.UnitHash} {
		if !safefs.DigestPattern.MatchString(h) {
			return fmt.Errorf("invalid installation digest")
		}
	}
	if len(m.Paths.DeniedPaths) > 64 {
		return fmt.Errorf("too many managed exclusions")
	}
	for _, path := range m.Paths.DeniedPaths {
		for _, required := range []string{m.Root, m.UnitDirectory} {
			if managedWithin(required, path) || managedWithin(path, required) {
				return fmt.Errorf("service exclusion overlaps installation authority")
			}
		}
	}
	release := filepath.Join(m.Root, "releases", m.Revision)
	if m.Paths.Executable != filepath.Join(release, "deploy") || m.Paths.Config != filepath.Join(release, "runtime.json") || m.Paths.Socket != filepath.Join(m.Paths.State, "api.sock") || managedWithin(m.Root, m.Paths.State) || managedWithin(m.Paths.State, m.Root) || managedWithin(m.UnitDirectory, m.Root) || managedWithin(m.Root, m.UnitDirectory) || managedWithin(m.UnitDirectory, m.Paths.State) || managedWithin(m.Paths.State, m.UnitDirectory) {
		return fmt.Errorf("installation layout changed")
	}
	_, err := RenderVerificationService(m.Paths)
	return err
}

func (j managedJournal) validate() error {
	i := j.Intent
	if isPausedRuntimeChange(i.Operation) {
		return validateDeliveryProviderJournal(j)
	}
	if i.Before == nil && i.After.DeliveryOwnerConfigHash != "" || i.Before != nil && i.Before.DeliveryOwnerConfigHash != i.After.DeliveryOwnerConfigHash {
		return fmt.Errorf("transition changed original delivery configuration")
	}
	if i.Before == nil && i.After.ReportCredentialHistory != nil || i.Before != nil && protocol.Hash(i.Before.ReportCredentialHistory) != protocol.Hash(i.After.ReportCredentialHistory) {
		return fmt.Errorf("installation transition changed retained credential exclusions")
	}
	if i.DeliveryProvider != nil {
		return fmt.Errorf("unexpected provider transition evidence")
	}
	if i.Operation == "set-led-output" {
		return validateLEDOutputJournal(j)
	}
	if i.Operation == "enable-commands" || i.Operation == "disable-commands" || isManagedCommandPolicyOperation(i.Operation) {
		return validateCommandJournal(j)
	}
	if i.Operation == "activate-delivery" || i.Operation == "deactivate-delivery" || i.Operation == "pause-delivery" {
		return validateDeliveryJournal(j)
	}
	if i.Version != managedVersion || !safefs.ValidID(i.ID, "n") || i.After.validate() != nil || (!isManagedPolicyOperation(i.Operation) && i.Operation != "enroll-bootstrap" && i.Operation != "setup" && i.Operation != "update" && i.Operation != "uninstall" && i.Operation != "migrate-schema" && i.Operation != "migrate-led" && i.Operation != "adopt-recovery" && i.Operation != "renew-telegram" && i.Operation != "upgrade-helper" && i.Operation != "adopt-signal" && i.Operation != "supervise-signal" && i.Operation != "migrate-signal-profiles") {
		return fmt.Errorf("invalid installation operation")
	}
	if i.Supersedes != nil {
		p := i.Supersedes
		if i.Operation != "uninstall" || p.Supersedes != nil || (p.Operation != "setup" && p.Operation != "update") || p.ID == i.ID || i.Before == nil {
			return fmt.Errorf("invalid interrupted-installation cancellation")
		}
		if err := (managedJournal{Intent: *p, Phase: "prepared"}).validate(); err != nil {
			return err
		}
		want := p.After
		want.Mode = "uninstalled"
		if protocol.Hash(i.After) != protocol.Hash(want) || protocol.Hash(*i.Before) != protocol.Hash(p.After) || i.AfterUnit != "" || (i.BeforeUnit != "" && i.BeforeUnit != p.BeforeUnit && i.BeforeUnit != p.AfterUnit) {
			return fmt.Errorf("cancellation changed interrupted installation authority")
		}
		if managedPhase(j.Phase) == 0 {
			return fmt.Errorf("invalid managed operation phase")
		}
		return nil
	}
	if i.Before == nil {
		if i.After.LEDOutput != nil {
			return fmt.Errorf("setup carries LED output history")
		}
		if i.After.CommandHistoryHash != "" || i.After.Commands != nil {
			return fmt.Errorf("setup carries command authority")
		}
		if i.After.DeliveryOwnerHash != "" || i.After.DeliveryActivation != nil {
			return fmt.Errorf("setup carries delivery authority")
		}
		if i.Operation != "setup" || i.BeforeUnit != "" || i.After.HelperHash != i.After.BinaryHash || i.After.RecoveryCommitHash != "" || i.After.TelegramRenewalID != "" || i.After.SignalVerification != nil || i.After.SignalProfiles != nil {
			return fmt.Errorf("missing prior installation")
		}
	} else {
		b := i.Before
		if protocol.Hash(b.LEDOutput) != protocol.Hash(i.After.LEDOutput) {
			return fmt.Errorf("LED output changes require explicit lifecycle")
		}
		if b.LEDOutput != nil && (b.ConfigHash != i.After.ConfigHash && !isManagedPolicyOperation(i.Operation) && i.Operation != "enroll-bootstrap" && i.Operation != "migrate-led" || b.Identity != i.After.Identity) {
			return fmt.Errorf("LED output history requires an explicit configuration handoff")
		}
		if protocol.Hash(b.FreshSignal) != protocol.Hash(i.After.FreshSignal) {
			return fmt.Errorf("managed operation changed fresh Signal service ownership")
		}
		nextSignal := i.Operation == "adopt-recovery" && b.SignalVerification != nil && i.After.SignalVerification != nil && i.After.SignalVerification.Operation == i.ID
		if nextSignal && (b.SignalVerification.ServiceRoot == "" || i.After.SignalVerification.ServiceRoot == "" || b.SignalVerification.Root == i.After.SignalVerification.Root || b.ConfigHash == i.After.ConfigHash || b.RuntimeHash == i.After.RuntimeHash) {
			return fmt.Errorf("invalid next Signal endpoint transition")
		}
		if i.Operation == "adopt-recovery" && b.SignalVerification != nil && !nextSignal {
			return fmt.Errorf("Signal recovery requires explicit next endpoint")
		}
		if i.Operation == "adopt-recovery" && b.CommandHistoryHash != "" {
			r, err := commandRecoveryBinding(*b, i.After)
			if err != nil || protocol.Hash(r) != i.After.CommandHistoryHash {
				return fmt.Errorf("invalid managed command recovery transition")
			}
		} else if b.CommandHistoryHash != i.After.CommandHistoryHash || protocol.Hash(b.Commands) != protocol.Hash(i.After.Commands) {
			return fmt.Errorf("command authority requires explicit command lifecycle")
		}
		if i.Operation == "adopt-recovery" && b.DeliveryOwnerHash != "" {
			r := i.After.DeliveryRecovery
			if r == nil || r.Before != b.Identity || protocol.Hash(r.Owner) != b.DeliveryOwnerHash || (b.DeliveryRecovery == nil && (r.Version != 1 || r.Owner.Identity != b.Identity)) || (b.DeliveryRecovery != nil && (r.Version != 2 || r.Owner != b.DeliveryRecovery.Owner)) {
				return fmt.Errorf("recovery adoption changed original delivery owner")
			}
		} else if protocol.Hash(b.DeliveryRecovery) != protocol.Hash(i.After.DeliveryRecovery) {
			return fmt.Errorf("managed operation changed delivery recovery lineage")
		}
		if b.Mode == "delivery" || b.DeliveryOwnerHash != i.After.DeliveryOwnerHash || protocol.Hash(b.DeliveryActivation) != protocol.Hash(i.After.DeliveryActivation) {
			return fmt.Errorf("delivery changes require explicit activation/deactivation")
		}
		if nextSignal && b.SignalProfiles != nil {
			if i.After.SignalProfiles == nil || i.After.SignalProfiles.Operation != i.ID || i.After.SignalProfiles.Conversations != b.SignalProfiles.Conversations {
				return fmt.Errorf("next Signal recovery changed conversation enrollment")
			}
		} else if i.Operation == "migrate-signal-profiles" {
			if b.Mode != "verification" || b.SignalProfiles != nil || b.SignalVerification == nil || b.SignalVerification.ServiceRoot == "" || i.After.SignalProfiles == nil || i.After.SignalProfiles.Operation != i.ID || b.RuntimeHash != i.After.RuntimeHash || b.ConfigHash != i.After.ConfigHash {
				return fmt.Errorf("invalid managed Signal profile migration")
			}
		} else if protocol.Hash(b.SignalProfiles) != protocol.Hash(i.After.SignalProfiles) {
			return fmt.Errorf("managed operation changed Signal profile binding")
		}
		if !nextSignal && i.Operation != "adopt-signal" && i.Operation != "supervise-signal" && protocol.Hash(b.SignalVerification) != protocol.Hash(i.After.SignalVerification) {
			return fmt.Errorf("managed operation changed Signal endpoint binding")
		}
		if i.Operation == "adopt-signal" {
			if b.Mode != "verification" || b.RecoveryCommitHash == "" || b.SignalVerification != nil || i.After.SignalVerification == nil || i.After.SignalVerification.Operation != i.ID || b.ConfigHash == i.After.ConfigHash || b.RuntimeHash == i.After.RuntimeHash || b.Schema != i.After.Schema || i.After.HelperRevision != i.After.Revision || i.After.HelperHash != i.After.BinaryHash {
				return fmt.Errorf("invalid managed Signal verification adoption")
			}
		}
		if i.Operation == "supervise-signal" {
			if b.Mode != "verification" || b.SignalVerification == nil || i.After.SignalVerification == nil || i.After.SignalVerification.Operation != i.ID || i.After.SignalVerification.ServiceRoot == "" || b.SignalVerification.Root != i.After.SignalVerification.Root || b.SignalVerification.ManifestHash != i.After.SignalVerification.ManifestHash || b.RuntimeHash != i.After.RuntimeHash || b.ConfigHash != i.After.ConfigHash {
				return fmt.Errorf("invalid managed Signal supervision transition")
			}
		}
		if i.Operation == "adopt-recovery" && i.After.TelegramRenewalID != "" || (i.Operation != "renew-telegram" && i.Operation != "adopt-recovery" && b.TelegramRenewalID != i.After.TelegramRenewalID) {
			return fmt.Errorf("managed operation changed Telegram renewal pin")
		}
		if i.Operation == "renew-telegram" {
			want := *b
			want.TelegramRenewalID = i.ID
			if b.Mode != "verification" || b.RecoveryCommitHash == "" || b.TelegramRenewalID == i.ID || protocol.Hash(want) != protocol.Hash(i.After) || i.BeforeUnit != i.AfterUnit {
				return fmt.Errorf("invalid managed Telegram renewal")
			}
		}
		if b.validate() != nil || i.Operation == "setup" || b.Installation != i.After.Installation || b.Root != i.After.Root || b.Paths.State != i.After.Paths.State || b.UnitDirectory != i.After.UnitDirectory || (i.Operation != "adopt-recovery" && (b.Identity != i.After.Identity || b.RecoveryCommitHash != i.After.RecoveryCommitHash)) || (!isManagedPolicyOperation(i.Operation) && i.Operation != "enroll-bootstrap" && i.Operation != "migrate-led" && i.Operation != "adopt-signal" && !nextSignal && b.ConfigHash != i.After.ConfigHash) {
			return fmt.Errorf("managed operation changed pinned authority")
		}
		if b.RecoveryCommitHash != "" && (i.Operation == "migrate-schema" || i.Operation == "migrate-led") {
			return fmt.Errorf("recovered runtime migration requires ancestor support")
		}
		if i.Operation == "adopt-recovery" {
			if b.Mode != "verification" || b.RecoveryCommitHash == i.After.RecoveryCommitHash || i.After.RecoveryCommitHash == "" || b.Identity.Worker != i.After.Identity.Worker || b.Identity.Recovery == i.After.Identity.Recovery || b.Schema != i.After.Schema || i.After.HelperRevision != i.After.Revision || i.After.HelperHash != i.After.BinaryHash {
				return fmt.Errorf("invalid managed recovery adoption")
			}
		} else if i.Operation == "migrate-schema" {
			if b.Schema != 9 || i.After.Schema != 10 || b.Mode != "verification" || i.After.HelperRevision != i.After.Revision || i.After.HelperHash != i.After.BinaryHash {
				return fmt.Errorf("invalid managed schema migration")
			}
		} else if isManagedPolicyOperation(i.Operation) || i.Operation == "enroll-bootstrap" {
			if b.Mode != "verification" || b.RecoveryCommitHash != "" || b.DeliveryOwnerHash != "" || b.Schema != i.After.Schema || b.ConfigHash == i.After.ConfigHash || i.After.HelperRevision != i.After.Revision || i.After.HelperHash != i.After.BinaryHash {
				return fmt.Errorf("invalid managed bootstrap enrollment")
			}
		} else if i.Operation == "migrate-led" {
			if b.Schema != i.After.Schema || b.Mode != "verification" || b.ConfigHash == i.After.ConfigHash || i.After.HelperRevision != i.After.Revision || i.After.HelperHash != i.After.BinaryHash {
				return fmt.Errorf("invalid managed LED mapping migration")
			}
		} else if i.Operation == "upgrade-helper" || i.Operation == "adopt-signal" || i.Operation == "supervise-signal" || i.Operation == "migrate-signal-profiles" {
			if b.Mode != "verification" || b.Schema != i.After.Schema || i.After.HelperRevision != i.After.Revision || i.After.HelperHash != i.After.BinaryHash {
				return fmt.Errorf("invalid managed helper upgrade")
			}
		} else if b.Schema != i.After.Schema || b.HelperHash != i.After.HelperHash || b.HelperRevision != i.After.HelperRevision {
			return fmt.Errorf("schema/helper changes require explicit migration")
		}
		if protocol.Hash(b.Paths.DeniedPaths) != protocol.Hash(i.After.Paths.DeniedPaths) {
			return fmt.Errorf("exclusion changes require explicit migration")
		}
		if (b.Mode == "verification" && managedDigest([]byte(i.BeforeUnit)) != b.UnitHash) || (b.Mode == "uninstalled" && i.BeforeUnit != "") {
			return fmt.Errorf("prior unit evidence mismatch")
		}
	}
	if i.Operation == "uninstall" {
		if i.After.Mode != "uninstalled" || i.AfterUnit != "" {
			return fmt.Errorf("uninstall carries activation authority")
		}
		b := *i.Before
		b.Mode = "uninstalled"
		if protocol.Hash(b) != protocol.Hash(i.After) {
			return fmt.Errorf("uninstall changed retained installation")
		}
	} else {
		if i.After.Mode != "verification" || managedDigest([]byte(i.AfterUnit)) != i.After.UnitHash {
			return fmt.Errorf("managed unit digest mismatch")
		}
		if i.Before != nil && i.Before.Revision == i.After.Revision && i.Operation != "renew-telegram" {
			return fmt.Errorf("update must allocate a new revision")
		}
		want, err := renderManagedUnit(i.After)
		if err != nil || want != i.AfterUnit {
			return fmt.Errorf("managed service is not verification-only")
		}
	}
	switch j.Phase {
	case "prepared", "stopped", "files", "published", "reloaded", "started", "complete":
	default:
		return fmt.Errorf("invalid managed operation phase")
	}
	return nil
}

func renderManagedUnit(m ManagedManifest) (string, error) {
	body, err := RenderVerificationService(m.Paths)
	if err != nil {
		return "", err
	}
	old := " notification-worker serve --local --verification --config " + systemdArgument(m.Paths.Config) + " --state-dir " + systemdArgument(m.Paths.State) + " --socket " + systemdArgument(m.Paths.Socket)
	next := " notification-worker __managed-serve --local --root " + systemdArgument(m.Root) + " --revision " + systemdArgument(m.Revision)
	if !strings.Contains(body, old) {
		return "", fmt.Errorf("verification unit template changed")
	}
	body = strings.Replace(body, old, next, 1)
	if m.Mode == "delivery" {
		body = strings.Replace(body, "Description=Verify staged deploy notification worker", "Description=Qualified deploy Telegram delivery worker", 1)
		body = strings.Replace(body, "RestrictAddressFamilies=AF_UNIX\n", "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6\n", 1)
	}
	if m.FreshSignal != nil {
		body = strings.Replace(body, "[Unit]\n", "[Unit]\nRequires="+freshSignalUnit+"\nAfter="+freshSignalUnit+"\n", 1)
	}
	if m.SignalVerification != nil && m.SignalVerification.ServiceRoot != "" {
		body = strings.Replace(body, "[Unit]\n", "[Unit]\nRequires="+signalServiceUnit+"\nAfter="+signalServiceUnit+"\n", 1)
	}
	return body, nil
}
