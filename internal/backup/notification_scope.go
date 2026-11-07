package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// VerificationNotificationScopeChange pins a gateway policy edit and its backup
// consumer. The selected reconciler determines whether staged LED changes or
// admitted command activity is allowed. A scope hash alone never authorizes
// replacing a producer or rewriting completed cutover evidence.
type VerificationNotificationScopeChange struct {
	Operation     string
	BeforeScope   string
	BeforeGateway string
	Desired       config.DesiredWorkers
}

type setupNotificationScope struct {
	Version       int                    `json:"version"`
	Operation     string                 `json:"operation"`
	BeforeGateway string                 `json:"before_gateway"`
	Before        WorkerSetupJournal     `json:"before"`
	After         WorkerSetupRequest     `json:"after"`
	Freshness     *setupFreshnessRuntime `json:"freshness,omitempty"`
	Reports       *setupProviderReports  `json:"reports,omitempty"`
}

type setupNotificationScopeReference struct {
	Operation string `json:"operation"`
	Receipt   string `json:"receipt"`
}

type setupNotificationScopeReceipt struct {
	Version int                    `json:"version"`
	Intent  setupNotificationScope `json:"intent"`
	Gateway string                 `json:"gateway"`
}

func (v setupNotificationScope) validate() error {
	b := v.Before
	if (v.Version < 1 || v.Version > 23) || !safefs.ValidID(v.Operation, "n") || !safefs.DigestPattern.MatchString(v.BeforeGateway) || b.NotificationScope != nil || ((v.Version == 1 || v.Version >= 6 && v.Version <= 15) && (b.Shared != nil || b.Schema != 1)) || (v.Version >= 2 && b.Schema != 1 && b.Schema != 2) || b.Stage != "complete" || !b.StateInitialized || b.Updating || b.RecoveryNeeded || b.SourceExpansion != nil || b.SourceRetirement != nil || b.SharedPolicyUpdate != nil || b.SharedPolicyExpansion != nil || b.TelegramRotation != nil || b.Producer == nil || b.Producer.Validate(b.WorkerID) != nil || b.Declaration == nil || b.Declaration.Desired == nil || b.Declaration.Desired.Notification == nil || b.Declaration.NotificationDesiredHash == "" || v.After.Declaration == nil || v.After.Declaration.Desired == nil || v.After.Declaration.Desired.Notification == nil {
		return fmt.Errorf("notification policy migration requires a completed backup reservation")
	}
	if err := validateSetupDesired(b.Declaration); err != nil {
		return err
	}
	if err := validateSetupDesired(v.After.Declaration); err != nil {
		return err
	}
	old, next := b.Declaration.Desired, v.After.Declaration.Desired
	if !freshnessScope(v.Version) && v.Version != 15 && !reflect.DeepEqual(old.Backup, next.Backup) {
		return fmt.Errorf("change notification policy separately from backup policy")
	}
	if !freshnessScope(v.Version) && v.Freshness != nil {
		return fmt.Errorf("unexpected backup runtime migration")
	}
	if !reportPolicyScope(v.Version) && v.Reports != nil {
		return fmt.Errorf("unexpected report credential migration")
	}
	if reportPolicyScope(v.Version) {
		validate := config.ValidateDeliveryProviderDeclaration
		if v.Version == 23 {
			validate = config.ValidateDerivedFreshnessDeclaration
		}
		if v.Version == 21 {
			validate = config.ValidateDeliveryProviderEnrollmentDeclaration
		}
		if v.Version == 17 {
			validate = config.ValidateDeliveryLimitsDeclaration
		}
		if v.Version == 19 {
			validate = config.ValidateDeliveryRoutesDeclaration
		}
		if v.Version == 18 {
			validate = config.ValidateDeliveryProducerDeclaration
		}
		if err := validate(*old, *next); err != nil {
			return err
		}
		if v.Version == 23 {
			if err := v.validateFreshness(); err != nil {
				return err
			}
		}
		if err := v.validateProviderReports(); err != nil {
			return err
		}
	} else if v.Version == 22 {
		if err := config.ValidateDeliveryDeclarationChange(*old, *next); err != nil {
			return err
		}
	} else if v.Version == 15 {
		if err := config.ValidateSharedReportRouteDeclaration(*old, *next); err != nil {
			return err
		}
	} else if freshnessScope(v.Version) {
		if err := config.ValidateDerivedFreshnessDeclaration(*old, *next); err != nil {
			return err
		}
		if err := v.validateFreshness(); err != nil {
			return err
		}
	} else if v.Freshness != nil {
		return fmt.Errorf("unexpected backup runtime migration")
	} else if v.Version == 13 {
		if err := config.ValidateVerificationCommandActivityDeclaration(*old, *next); err != nil {
			return err
		}
	} else if v.Version == 12 {
		if err := config.ValidateVerificationCommandDeclaration(*old, *next); err != nil {
			return err
		}
	} else if v.Version == 11 {
		if err := config.ValidateInitialCommandDeclaration(*old, *next); err != nil {
			return err
		}
	} else if v.Version == 2 {
		copy := *next.Notification
		if copy.Commands.Enabled == old.Notification.Commands.Enabled {
			return fmt.Errorf("command activity requires changed enabled permission")
		}
		copy.Commands.Enabled = old.Notification.Commands.Enabled
		if !reflect.DeepEqual(copy, *old.Notification) {
			return fmt.Errorf("change command enablement separately from other notification policy")
		}
	} else if v.Version == 3 {
		copy := *next.Notification
		if old.Notification.Commands.Enabled || copy.Commands.Enabled || reflect.DeepEqual(old.Notification.Commands.Permissions, copy.Commands.Permissions) {
			return fmt.Errorf("command permission change requires disabled intake and changed permissions")
		}
		copy.Commands.Permissions = old.Notification.Commands.Permissions
		if !reflect.DeepEqual(copy, *old.Notification) {
			return fmt.Errorf("change command permissions separately from other notification policy")
		}
	} else if v.Version == 4 {
		copy := *next.Notification
		if old.Notification.Commands.Enabled || copy.Commands.Enabled || reflect.DeepEqual(old.Notification.Commands, copy.Commands) {
			return fmt.Errorf("command enrollment requires disabled intake and changed policy")
		}
		copy.Commands = old.Notification.Commands
		if !reflect.DeepEqual(copy, *old.Notification) {
			return fmt.Errorf("change command enrollment separately from other notification policy")
		}
	} else if v.Version == 10 {
		copy := *next.Notification
		if reflect.DeepEqual(copy.Providers, old.Notification.Providers) {
			return fmt.Errorf("shared provider change requires new credential references")
		}
		copy.Providers = old.Notification.Providers
		if !reflect.DeepEqual(copy, *old.Notification) {
			return fmt.Errorf("change shared providers separately from other policy")
		}
	} else if v.Version == 8 {
		copy := *next.Notification
		if reflect.DeepEqual(copy.Routes, old.Notification.Routes) {
			return fmt.Errorf("shared routing requires changed routes")
		}
		copy.Routes = old.Notification.Routes
		if !reflect.DeepEqual(copy, *old.Notification) {
			return fmt.Errorf("change shared routes separately from other policy")
		}
	} else if v.Version == 7 || v.Version == 9 {
		copy := *next.Notification
		if reflect.DeepEqual(copy.Producers, old.Notification.Producers) {
			return fmt.Errorf("shared freshness requires changed producer policy")
		}
		copy.Producers = old.Notification.Producers
		if !reflect.DeepEqual(copy, *old.Notification) {
			return fmt.Errorf("change shared producer timing separately from other policy")
		}
	} else if v.Version == 6 {
		copy := *next.Notification
		if copy.Limits == old.Notification.Limits {
			return fmt.Errorf("shared limits require changed budgets")
		}
		copy.Limits = old.Notification.Limits
		if !reflect.DeepEqual(copy, *old.Notification) {
			return fmt.Errorf("change shared limits separately from other policy")
		}
	} else if v.Version == 5 {
		copy := *next.Notification
		if copy.StatusLEDs.Enabled == old.Notification.StatusLEDs.Enabled {
			return fmt.Errorf("delivery LED output requires a changed enabled setting")
		}
		copy.StatusLEDs.Enabled = old.Notification.StatusLEDs.Enabled
		if !reflect.DeepEqual(copy, *old.Notification) {
			return fmt.Errorf("change delivery LED output separately from other policy")
		}
	} else {
		copy := *next.Notification
		bindingChange := !reflect.DeepEqual(next.Notification.StatusLEDs.Bindings, old.Notification.StatusLEDs.Bindings)
		outputChange := next.Notification.StatusLEDs.Enabled != old.Notification.StatusLEDs.Enabled
		if bindingChange == outputChange || bindingChange && !old.Notification.StatusLEDs.Enabled {
			return fmt.Errorf("change either enabled LED bindings or LED output permission separately")
		}
		copy.StatusLEDs.Bindings = old.Notification.StatusLEDs.Bindings
		copy.StatusLEDs.Enabled = old.Notification.StatusLEDs.Enabled
		if !reflect.DeepEqual(copy, *old.Notification) {
			return fmt.Errorf("verification scope migration currently accepts only LED bindings or output permission")
		}
	}
	d := *v.After.Declaration
	d.Desired, d.DesiredHash, d.NotificationDesiredHash = b.Declaration.Desired, b.Declaration.DesiredHash, b.Declaration.NotificationDesiredHash
	if v.Version == 15 {
		if d.NotificationRoute != next.Backup.Notifications.Route {
			return fmt.Errorf("backup reporting route differs from declaration")
		}
		d.NotificationRoute = b.Declaration.NotificationRoute
	}
	if !reflect.DeepEqual(d, *b.Declaration) {
		return fmt.Errorf("notification scope migration changed backup paths, identity or artifacts")
	}
	prior := v.After
	prior.Declaration = &d
	if freshnessScope(v.Version) {
		prior.Policy.Freshness = old.Backup.Freshness
	}
	if workerSetupRequestHash(prior) != b.RequestHash || v.After.Policy.Notifications.Mode != "shared" || v.After.Recover || v.After.Fenced || v.After.Operation != "apply" {
		return fmt.Errorf("notification scope migration lost the exact backup setup request")
	}
	return nil
}

func checkSetupNotificationScope(j WorkerSetupJournal) error {
	if j.NotificationScope == nil {
		return nil
	}
	v := *j.NotificationScope
	if err := v.validate(); err != nil {
		return err
	}
	copy := j
	copy.NotificationScope = nil
	if !reflect.DeepEqual(copy, v.Before) {
		return fmt.Errorf("pending notification policy migration lost backup authority")
	}
	return nil
}

func verifyNotificationScopeHistory(root *safefs.Directory, j WorkerSetupJournal) error {
	ref := j.NotificationScopeHistory
	if ref == nil {
		return nil
	}
	if !safefs.ValidID(ref.Operation, "n") || !safefs.DigestPattern.MatchString(ref.Receipt) || j.Declaration == nil {
		return fmt.Errorf("invalid notification policy history reference")
	}
	var receipt setupNotificationScopeReceipt
	if err := root.ReadJSON(".local/state/deploy/backup-worker/notification-scope-"+ref.Operation+".json", &receipt); err != nil {
		return err
	}
	if receipt.Intent.Version == 22 && receipt.Gateway != receipt.Intent.BeforeGateway {
		return fmt.Errorf("declaration receipt changed gateway")
	}
	if receipt.Version != 1 || receipt.Intent.validate() != nil || receipt.Intent.Operation != ref.Operation || notify.Hash(receipt) != ref.Receipt || !safefs.DigestPattern.MatchString(receipt.Gateway) || receipt.Intent.Before.WorkerID != j.WorkerID || receipt.Intent.Before.ArchiveID != j.ArchiveID || receipt.Intent.After.Declaration.NotificationDesiredHash != j.Declaration.NotificationDesiredHash {
		return fmt.Errorf("notification policy history lost retained authority")
	}
	return nil
}

// InspectVerificationNotificationScope verifies the backup half of a completed
// gateway operation without replaying it. Later backup-only edits may retain the
// same gateway scope; those are not a reason to restore an old backup snapshot.
func InspectVerificationNotificationScope(home, operation, beforeGateway, scope string) (string, bool, error) {
	return inspectVerificationNotificationScope(home, operation, beforeGateway, scope, nil)
}

// InspectRetainedDeliveryOutputScope can additionally verify earlier receipts
// through an exact chain of output-only changes. It never restores an earlier
// backup snapshot and does not accept command or backup policy changes as edges.
func InspectRetainedDeliveryOutputScope(home, operation, beforeGateway, scope string) (string, bool, error) {
	return inspectVerificationNotificationScope(home, operation, beforeGateway, scope, []int{5})
}

// InspectRetainedDeliveryPolicyScope also follows exact credential-only report
// exclusion migrations; it never treats unrelated backup edits as ancestry.
func InspectRetainedDeliveryPolicyScope(home, operation, beforeGateway, scope string) (string, bool, error) {
	return inspectVerificationNotificationScope(home, operation, beforeGateway, scope, []int{5, 16, 17, 18, 19, 20, 21, 22, 23})
}

func inspectVerificationNotificationScope(home, operation, beforeGateway, scope string, retainedKinds []int) (string, bool, error) {
	if !safefs.ValidID(operation, "n") || !safefs.DigestPattern.MatchString(beforeGateway) || !safefs.DigestPattern.MatchString(scope) {
		return "", false, fmt.Errorf("invalid notification scope inspection")
	}
	root, err := safefs.Open(home)
	if err != nil {
		return "", false, err
	}
	defer root.Close()
	lease, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", false, false)
	if err != nil {
		return "", false, err
	}
	defer lease.Close()
	var j WorkerSetupJournal
	if err = root.ReadJSON(backupSetupFile, &j); err != nil {
		return "", false, err
	}
	if err = checkSetupNotificationScope(j); err != nil {
		return "", false, err
	}
	if err = verifyNotificationScopeHistory(root, j); err != nil {
		return "", false, err
	}
	if j.NotificationScope != nil {
		v := j.NotificationScope
		if v.Operation != operation || v.BeforeGateway != beforeGateway || v.After.Declaration.NotificationDesiredHash != scope {
			return "", false, fmt.Errorf("another notification policy migration is pending")
		}
		return "", false, nil
	}
	if j.Stage != "complete" || j.Updating || j.RecoveryNeeded || j.SourceExpansion != nil || j.SourceRetirement != nil || j.Activity != nil && (!j.Activity.Complete || j.Activity.Disabled) {
		return "", false, fmt.Errorf("finish pending backup lifecycle before notification admission")
	}
	if err = verifySetupSourceKeys(root, j); err != nil {
		return "", false, err
	}
	for depth := 0; len(retainedKinds) > 0 && j.NotificationScopeHistory != nil && j.NotificationScopeHistory.Operation != operation; depth++ {
		if depth >= 128 {
			return "", false, fmt.Errorf("retained output scope exceeds review bound")
		}
		if err = verifyNotificationScopeHistory(root, j); err != nil {
			return "", false, err
		}
		var edge setupNotificationScopeReceipt
		if err = root.ReadJSON(".local/state/deploy/backup-worker/notification-scope-"+j.NotificationScopeHistory.Operation+".json", &edge); err != nil {
			return "", false, err
		}
		if !slices.Contains(retainedKinds, edge.Intent.Version) {
			return "", false, fmt.Errorf("retained scope requires output-only ancestry")
		}
		j = edge.Intent.Before
	}
	if err = verifyNotificationScopeHistory(root, j); err != nil {
		return "", false, err
	}
	if j.NotificationScopeHistory == nil || j.NotificationScopeHistory.Operation != operation || j.Declaration == nil || j.Declaration.NotificationDesiredHash != scope {
		return "", false, fmt.Errorf("completed gateway has no matching backup policy receipt")
	}
	var receipt setupNotificationScopeReceipt
	if err = root.ReadJSON(".local/state/deploy/backup-worker/notification-scope-"+operation+".json", &receipt); err != nil {
		return "", false, err
	}
	if receipt.Intent.BeforeGateway != beforeGateway {
		return "", false, fmt.Errorf("gateway policy predecessor changed")
	}
	if reportPolicyScope(receipt.Intent.Version) {
		var completion providerReportCompletion
		err := root.ReadJSON(".local/state/deploy/backup-worker/notification-report-"+operation+".json", &completion)
		if os.IsNotExist(err) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		if completion != (providerReportCompletion{1, notify.Hash(receipt.Intent), receipt.Gateway}) {
			return "", false, fmt.Errorf("report exclusion completion changed")
		}
	}
	if freshnessScope(receipt.Intent.Version) || reportPolicyScope(receipt.Intent.Version) {
		p, e := declarationPaths(home, j.Declaration)
		if e != nil {
			return "", false, e
		}
		d, e := safefs.Open(p.State)
		if e != nil {
			return "", false, e
		}
		defer d.Close()
		var gate installationGate
		if e = d.ReadJSON("installation.pending", &gate); e == nil {
			if gate.Operation != operation {
				return "", false, fmt.Errorf("another backup installation is pending")
			}
			return "", false, nil
		} else if !os.IsNotExist(e) {
			return "", false, e
		}
	}
	return receipt.Gateway, true, nil
}

// Caller supplies the gateway's canonical idempotent migration and inspection.
// The backup setup lease spans that transition and the final scope publication.
// No runtime, source key, archive data or production admission is changed here.
func ReconcileVerificationNotificationScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 1)
}

// ReconcileCommandNotificationScope coordinates command enable/disable without
// changing backup runtime or the original completed delivery handoff.
func ReconcileCommandNotificationScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 2)
}

// ReconcileCommandPolicyScope coordinates a reviewed permission reduction with
// the backup declaration. The gateway independently enforces narrowing.
func ReconcileCommandPolicyScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 3)
}

// ReconcileCommandEnrollmentScope retains command alias/conversation reductions
// with the backup declaration. The gateway independently checks the subset.
func ReconcileCommandEnrollmentScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 4)
}

// ReconcileDeliveryLEDOutputScope retains an output-only change without replacing
// backup ownership or the original qualified reporting handoff.
func ReconcileDeliveryLEDOutputScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 5)
}

func reconcileNotificationScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error), version int, boundaries ...func(string) error) (string, error) {
	return reconcileNotificationScopeReports(ctx, home, request, migrate, version, nil, boundaries...)
}

func reconcileNotificationScopeReports(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error), version int, reports *providerScopeDependencies, boundaries ...func(string) error) (string, error) {
	boundary := func(at string) error {
		if len(boundaries) == 1 {
			return boundaries[0](at)
		}
		return nil
	}
	if reportPolicyScope(version) && reports == nil {
		return "", fmt.Errorf("provider scope dependencies unavailable")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !safefs.ValidID(request.Operation, "n") || !safefs.DigestPattern.MatchString(request.BeforeScope) || !safefs.DigestPattern.MatchString(request.BeforeGateway) || migrate == nil {
		return "", fmt.Errorf("exact notification scope predecessor required")
	}
	root, err := safefs.Open(home)
	if err != nil {
		return "", err
	}
	defer root.Close()
	lease, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	if err != nil {
		return "", err
	}
	defer lease.Close()
	d, err := root.Sub(".local/state/deploy/backup-worker")
	if err != nil {
		return "", err
	}
	defer d.Close()
	var j WorkerSetupJournal
	if err = d.ReadJSON("setup.json", &j); err != nil {
		return "", err
	}
	if err = checkSetupProducerReservation(d, &j); err != nil {
		return "", err
	}
	if err = checkSetupNotificationScope(j); err != nil {
		return "", err
	}
	if err = verifyNotificationScopeHistory(root, j); err != nil {
		return "", err
	}
	newHash, err := request.Desired.Fingerprint()
	if err != nil {
		return "", err
	}
	newScope, err := request.Desired.NotificationFingerprint()
	if err != nil {
		return "", err
	}
	name := "notification-scope-" + request.Operation + ".json"
	var receipt setupNotificationScopeReceipt
	readErr := d.ReadJSON(name, &receipt)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", readErr
	}
	if readErr == nil {
		v := receipt.Intent
		if reportPolicyScope(version) && (v.Reports == nil || !reflect.DeepEqual(v.Reports.Credentials, reports.credentials)) {
			return "", fmt.Errorf("completed credential exclusion plan changed")
		}
		if receipt.Version != 1 || v.Version != version || v.validate() != nil || !safefs.DigestPattern.MatchString(receipt.Gateway) || v.Operation != request.Operation || v.BeforeGateway != request.BeforeGateway || v.Before.Declaration.NotificationDesiredHash != request.BeforeScope || v.After.Declaration.DesiredHash != newHash || v.After.Declaration.NotificationDesiredHash != newScope {
			return "", fmt.Errorf("notification scope completion receipt changed")
		}
		if j.NotificationScope == nil {
			want := v.Before
			want.Declaration, want.RequestHash, want.NotificationScopeHistory = v.After.Declaration, workerSetupRequestHash(v.After), &setupNotificationScopeReference{request.Operation, notify.Hash(receipt)}
			if freshnessScope(v.Version) {
				want.OwnedFiles = v.freshnessOwnedFiles()
			}
			if reportPolicyScope(v.Version) {
				want = v.providerReportsAfter(want, receipt.Gateway)
			}
			if !reflect.DeepEqual(want, j) {
				return "", fmt.Errorf("notification scope completion no longer matches backup setup")
			}
			if v.Version == 14 {
				// A lost final reply can leave only the runtime gate pending.
				// Reuse its lease before verifying and removing that gate.
				paths, e := declarationPaths(home, j.Declaration)
				if e != nil {
					return "", e
				}
				stateDir, e := safefs.Open(paths.State)
				if e != nil {
					return "", e
				}
				defer stateDir.Close()
				lease, e := stateDir.Lock("schema.lock", true, false)
				if e != nil {
					return "", e
				}
				defer lease.Close()
				if err := v.finishFreshnessGate(root); err != nil {
					return "", err
				}
			}
			if reportPolicyScope(v.Version) {
				if err := v.finishProviderReports(ctx, root, receipt.Gateway, reports); err != nil {
					return "", err
				}
			}
			return receipt.Gateway, nil
		}
	}
	if j.NotificationScope == nil {
		if j.Declaration == nil || j.Declaration.NotificationDesiredHash != request.BeforeScope || j.Declaration.Home != home || j.Declaration.UID != os.Getuid() {
			return "", fmt.Errorf("backup notification scope predecessor differs")
		}
		if err = verifySetupSourceKeys(root, j); err != nil {
			return "", err
		}
		if err = validateSourceRetirementHistory(j); err != nil {
			return "", err
		}
		if err = requireSettledDeclaredActivation(home); err != nil {
			return "", err
		}
		if j.Activity != nil && (!j.Activity.Complete || j.Activity.Disabled) {
			return "", fmt.Errorf("finish backup activity before notification policy migration")
		}
		var lifecycle workerLifecycleJournal
		if e := d.ReadJSON("lifecycle.json", &lifecycle); e == nil && (lifecycle.Stage != "complete" || lifecycle.Operation == "uninstall") {
			return "", fmt.Errorf("finish backup lifecycle before notification policy migration")
		} else if e != nil && !os.IsNotExist(e) {
			return "", e
		}
		c, e := loadInstalledWorker(home)
		if e != nil {
			return "", e
		}
		paths, e := declarationPaths(home, j.Declaration)
		if e != nil {
			return "", e
		}
		if _, _, e = inspectSetupSourcePins(root, j, c, paths); e != nil {
			return "", e
		}
		after := WorkerSetupRequest{Schema: 1, Operation: "apply", Policy: c.Policy, Image: c.Image, RelayImage: c.RelayImage}
		decl := *j.Declaration
		decl.Desired, decl.DesiredHash, decl.NotificationDesiredHash = &request.Desired, newHash, newScope
		if version == 15 {
			if request.Desired.Backup == nil {
				return "", fmt.Errorf("backup report route declaration missing")
			}
			decl.NotificationRoute = request.Desired.Backup.Notifications.Route
		}
		after.Declaration = &decl
		for _, s := range c.Sources {
			known, e := readBackupOwnedFile(root, paths.OwnedPath(s.KnownHosts), 16384)
			if e != nil {
				return "", e
			}
			after.Sources = append(after.Sources, SetupSource{s.Alias, s.Host, s.User, s.Port, string(known)})
		}
		v := setupNotificationScope{Version: version, Operation: request.Operation, BeforeGateway: request.BeforeGateway, Before: j, After: after}
		if freshnessScope(version) {
			if j.Declaration.Desired == nil {
				return "", fmt.Errorf("derived freshness requires the retained backup declaration")
			}
			if err = config.ValidateDerivedFreshnessDeclaration(*j.Declaration.Desired, request.Desired); err != nil {
				return "", err
			}
			v.After.Policy.Freshness = request.Desired.Backup.Freshness
			if err = v.prepareFreshness(root, c, paths); err != nil {
				return "", err
			}
		}
		if reportPolicyScope(version) {
			if version == 23 {
				if err = v.prepareFreshnessCapture(ctx, home, reports); err != nil {
					return "", err
				}
			}
			if err = v.prepareProviderReports(ctx, root, c, reports); err != nil {
				return "", err
			}
		}
		if err = v.validate(); err != nil {
			return "", err
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		var retained setupNotificationScope
		if err = json.Unmarshal(raw, &retained); err != nil {
			return "", err
		}
		j.NotificationScope = &retained
	}
	v := *j.NotificationScope
	if v.Version != version || v.Operation != request.Operation || v.BeforeGateway != request.BeforeGateway || v.Before.Declaration.NotificationDesiredHash != request.BeforeScope || v.After.Declaration.DesiredHash != newHash || v.After.Declaration.NotificationDesiredHash != newScope {
		return "", fmt.Errorf("resume the exact retained notification policy migration")
	}
	if reportPolicyScope(version) && !reflect.DeepEqual(v.Reports.Credentials, reports.credentials) {
		return "", fmt.Errorf("resume the exact credential exclusion plan")
	}
	if err = verifySetupSourceKeys(root, j); err != nil {
		return "", err
	}
	if err = validateSourceRetirementHistory(j); err != nil {
		return "", err
	}
	for path, hash := range j.OwnedFiles {
		body, e := readBackupOwnedFile(root, path, 8<<20)
		if e == nil && freshnessScope(version) && v.freshnessFileMatches(path, body) {
			continue
		}
		if e == nil && reportPolicyScope(version) && v.Reports.matches(path, body, v.providerReportPaths()) {
			continue
		}
		if e != nil || sharedSetupDigest(body) != hash {
			return "", fmt.Errorf("backup runtime changed during notification policy migration")
		}
	}
	if freshnessScope(version) || reportPolicyScope(version) {
		paths, e := declarationPaths(home, j.Declaration)
		if e != nil {
			return "", e
		}
		stateDir, e := safefs.Open(paths.State)
		if e != nil {
			return "", e
		}
		var gate installationGate
		e = stateDir.ReadJSON("installation.pending", &gate)
		stateDir.Close()
		if e == nil && gate.Operation != request.Operation {
			return "", fmt.Errorf("another backup installation is pending")
		}
		if e != nil && !os.IsNotExist(e) {
			return "", e
		}
	}
	state, err := openInstalledBackupStateWithLease(home, false, false, freshnessScope(version) || reportPolicyScope(version))
	if err != nil {
		return "", err
	}
	if version == 1 || version >= 6 && version <= 15 {
		err = requirePendingSharedSetup(state, &j)
	} else {
		err = requireCommandScopeOwner(state, j)
	}
	var activity string
	if err == nil {
		err = state.get("installation", &activity)
	}
	state.Close()
	if err != nil {
		return "", err
	}
	if activity != "staged-awaiting-qualification" && !((version >= 2 && version <= 5 || reportPolicyScope(version) || version == 22) && activity == "active") {
		return "", fmt.Errorf("notification scope migration requires the supported staged or active backup lifecycle")
	}
	if err = d.WriteJSON("setup.json", j, true); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = boundary("scope-intent"); err != nil {
		return "", err
	}
	var releaseScope func()
	if freshnessScope(version) || reportPolicyScope(version) {
		release, e := v.lockFreshness(root)
		if e != nil {
			return "", e
		}
		releaseScope = release
		defer func() {
			if releaseScope != nil {
				releaseScope()
			}
		}()
		if err = boundary("backup-gated"); err != nil {
			return "", err
		}
	}
	if reportPolicyScope(version) {
		if version == 23 {
			if err = v.stageFreshnessCapture(ctx, home, reports, boundary); err != nil {
				return "", err
			}
		}
		if err = v.stageProviderReports(ctx, root, reports, boundary); err != nil {
			return "", err
		}
	}
	gateway, err := migrate(*j.Producer)
	if err != nil {
		return "", err
	}
	if version == 22 && gateway != v.BeforeGateway {
		return "", fmt.Errorf("declaration continuation changed gateway ownership")
	}
	if !safefs.DigestPattern.MatchString(gateway) {
		return "", fmt.Errorf("notification scope migration has no canonical gateway receipt")
	}
	if err = boundary("gateway-complete"); err != nil {
		return "", err
	}
	if freshnessScope(version) {
		if err = v.publishFreshness(root, boundary); err != nil {
			return "", err
		}
	}
	if reportPolicyScope(version) {
		if err = v.inspectProviderReports(ctx, root, gateway, reports, true); err != nil {
			return "", err
		}
	}
	next := setupNotificationScopeReceipt{1, v, gateway}
	if readErr == nil {
		if !reflect.DeepEqual(next, receipt) {
			return "", fmt.Errorf("notification scope completion changed on retry")
		}
	} else if err = d.WriteJSON(name, next, false); err != nil {
		return "", err
	}
	if err = boundary("scope-receipt"); err != nil {
		return "", err
	}
	j.Declaration, j.RequestHash = v.After.Declaration, workerSetupRequestHash(v.After)
	if freshnessScope(version) {
		j.OwnedFiles = v.freshnessOwnedFiles()
	}
	if reportPolicyScope(version) {
		j = v.providerReportsAfter(j, gateway)
	}
	j.NotificationScope, j.NotificationScopeHistory = nil, &setupNotificationScopeReference{request.Operation, notify.Hash(next)}
	if err = d.WriteJSON("setup.json", j, true); err != nil {
		return "", err
	}
	if err = boundary("scope-complete"); err != nil {
		return "", err
	}
	if version == 14 {
		if err = v.finishFreshnessGate(root); err != nil {
			return "", err
		}
	}
	if reportPolicyScope(version) {
		releaseScope()
		releaseScope = nil
		if err = v.finishProviderReports(ctx, root, gateway, reports); err != nil {
			return "", err
		}
	}
	return gateway, nil
}

// ReconcileVerificationLimitsScope retains a budget change before delivery admission.
func ReconcileVerificationLimitsScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 6)
}

// ReconcileVerificationFreshnessScope retains timing policy before delivery admission.
func ReconcileVerificationFreshnessScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 7)
}

// ReconcileVerificationRoutesScope retains route policy before delivery admission.
func ReconcileVerificationRoutesScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 8)
}

func ReconcileVerificationProducerPolicyScope(ctx context.Context, home string, request VerificationNotificationScopeChange, change func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, change, 9)
}

func ReconcileVerificationProvidersScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 10)
}

func ReconcileVerificationCommandsScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 11)
}

func ReconcileVerificationCommandPolicyScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 12)
}

func ReconcileVerificationCommandActivityScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 13)
}

func ReconcileVerificationReportRouteScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 15)
}

func ReconcileDeliveryDeclarationScope(ctx context.Context, home string, r VerificationNotificationScopeChange, _ []string, change func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, r, change, 22)
}
