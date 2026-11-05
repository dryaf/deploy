package install

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

// ActivationReview is an ephemeral inventory of prerequisites. No combination
// of satisfied checks can authorize production; the managed activation protocol
// must consume fresh evidence in its own journaled transition.
type ActivationReview struct {
	EffectiveConfigHash       string                  `json:"effective_config_hash"`
	Version                   int                     `json:"version"`
	ManifestHash              string                  `json:"manifest_hash"`
	Identity                  protocol.Identity       `json:"identity"`
	Revision                  string                  `json:"revision"`
	CheckedAt                 time.Time               `json:"checked_at"`
	Checks                    []worker.AdmissionCheck `json:"checks"`
	ActivationAllowed         bool                    `json:"activation_allowed"`
	DeliveryEnabled           bool                    `json:"delivery_enabled"`
	IntakeEnabled             bool                    `json:"intake_enabled"`
	ExecutionEnabled          bool                    `json:"execution_enabled"`
	RequiresActivationRecheck bool                    `json:"requires_activation_recheck"`
}

func ReviewActivation(ctx context.Context, root string) (ActivationReview, error) {
	return reviewActivation(ctx, root, userManagedServices{}, userSignalServiceManager{}, time.Now)
}

func reviewActivation(ctx context.Context, root string, services managedServices, signalManager signalServiceManager, now func() time.Time) (out ActivationReview, err error) {
	if services == nil || signalManager == nil || now == nil {
		return out, fmt.Errorf("activation review dependencies unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	d, err := privateManagedDir(root, true)
	if err != nil {
		return out, err
	}
	defer d.Close()
	operation, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return out, err
	}
	defer operation.Close()
	m, c, err := qualificationInstallation(d, root)
	if err != nil {
		return out, err
	}
	out = ActivationReview{Version: 1, ManifestHash: protocol.Hash(m), Identity: m.Identity, Revision: m.Revision, RequiresActivationRecheck: true}
	add := func(scope, stage, state, reason string) {
		out.Checks = append(out.Checks, worker.AdmissionCheck{Scope: scope, Stage: stage, State: state, Reason: reason})
	}
	add("installation", "artifacts", "satisfied", "completed_verification_revision_pinned")
	if deliveryPolicy(m, c) == nil {
		add("installation", "activation", "satisfied", "explicit_telegram_delivery_transition_available")
	} else {
		add("installation", "activation", "blocked", "configured_activation_scope_not_implemented")
	}
	qualificationManifest := m
	if m.Mode == "delivery" {
		_, qualificationManifest, err = readDeliveryAdmission(d, m, c)
		if err != nil {
			return ActivationReview{}, err
		}
		add("delivery", "mode", "satisfied", "retained_telegram_delivery_activation")
	}

	// Read the entire retained history, even if credentials are unavailable.
	// A missing committed tail or an interrupted acknowledgement cannot be hidden
	// by selecting an older successful report.
	history, err := readQualificationHistory(d, m.Installation)
	if err != nil {
		return ActivationReview{}, err
	}
	qualification := func() {
		state, reason := "blocked", "qualification_evidence_missing"
		if history.Pending {
			reason = "qualification_acknowledgement_pending"
		} else if len(history.Records) > 0 {
			record := history.Records[len(history.Records)-1]
			binding, e := qualificationBinding(qualificationManifest, c)
			at := now().UTC()
			if e != nil {
				reason = "qualification_inputs_unavailable"
			} else if record.Report.matches(binding, c, at) != nil || at.Before(record.RecordedAt) {
				reason = "qualification_binding_or_time_changed"
			} else if !record.Report.evidenceComplete(c) {
				reason = "qualification_evidence_incomplete"
			} else {
				state, reason = "satisfied", "current_operator_evidence_only"
			}
		}
		add("qualification", "evidence", state, reason)
	}

	// The exclusive installation lease prevents cooperating automatic restarts.
	// Never stop a service as a side effect of asking for a review.
	installation, leaseErr := d.Lock("installation.lock", true, false)
	if leaseErr == nil {
		defer installation.Close()
	} else if !errors.Is(leaseErr, syscall.EWOULDBLOCK) {
		return ActivationReview{}, leaseErr
	}
	manager, inspectErr := inspectManagedService(ctx, services, m, false)
	drained := leaseErr == nil && inspectErr == nil && (manager.Active == "inactive" || manager.Active == "failed")
	if drained {
		add("runtime", "drain", "satisfied", "gateway_inactive_installation_lease_held")
	} else {
		add("runtime", "drain", "blocked", "gateway_must_be_stopped_and_drained")
	}
	renewals := map[string]string{}
	if m.TelegramRenewalID != "" {
		receipt, e := readManagedTelegramReceipt(d, m)
		if e != nil {
			return ActivationReview{}, e
		}
		renewals[receipt.Renewal.Binding.Account] = receipt.Renewal.RenewalHash
	}
	var state worker.ManagedAdmissionReview
	stateValid := false
	if drained {
		state, err = worker.ReviewManagedAdmission(ctx, m.Paths.State, c.Worker, m.Identity, m.RecoveryCommitHash, renewals)
		if err == nil {
			stateValid = true
			out.Checks = append(out.Checks, state.Checks...)
			add("runtime", "state", "satisfied", "worker_state_reviewed_under_exclusive_lease")
		} else {
			add("runtime", "state", "blocked", "worker_state_busy_incomplete_or_changed")
		}
	} else {
		add("runtime", "state", "blocked", "worker_review_requires_gateway_drain")
	}
	c, err = managedEffectiveRuntime(d, m, c)
	if err != nil {
		return ActivationReview{}, err
	}
	out.EffectiveConfigHash = protocol.Hash(c)
	if m.SignalProfiles != nil {
		add("signal", "profiles", "satisfied", "immutable_signal_profile_transition_verified")
	}
	for name, p := range c.Providers {
		scope := "provider/" + name
		switch p.Kind {
		case "telegram":
			if provider.CheckTelegramProfile(p) != nil {
				add(scope, "binding", "blocked", "telegram_credentials_or_destination_changed")
			} else {
				add(scope, "binding", "satisfied", "local_telegram_destination_matches")
			}
		case "signal":
			state, reason := "blocked", "signal_outbound_profile_requires_explicit_migration"
			if p.Conversation != nil && c.Signal != nil && p.Conversation.Account == c.Signal.Account && p.Socket == c.Signal.Socket && p.RelayIdentity == c.Signal.InboxIdentity {
				state, reason = "satisfied", "signal_profile_matches_enrolled_endpoint"
			}
			add(scope, "binding", state, reason)
		}
	}
	if c.Telegram != nil {
		state, reason := "satisfied", "local_telegram_account_matches"
		if provider.CheckTelegramAccount(*c.Telegram) != nil {
			state, reason = "blocked", "telegram_account_credentials_changed"
		}
		add("intake/telegram/"+c.Telegram.Account, "credentials", state, reason)
		add("intake/telegram/"+c.Telegram.Account, "receiver_ownership", "blocked", "external_receiver_exclusion_requires_activation_protocol")
	}
	if qualificationUsesSignal(c) {
		if m.SignalVerification == nil || c.Signal == nil {
			add("signal", "endpoint", "blocked", "managed_signal_endpoint_required")
		} else {
			binding := m.SignalVerification.SignalVerificationReference
			cursor, found := state.Signal[c.Signal.Account]
			if !stateValid || !found || cursor.Handoff.Target.Identity != c.Signal.InboxIdentity {
				add("signal", "gateway_cursor", "blocked", "signal_gateway_cursor_not_verified")
			} else {
				add("signal", "gateway_cursor", "satisfied", "signal_gateway_cursor_matches_endpoint")
			}
			if binding.ServiceRoot == "" {
				add("signal", "supervision", "blocked", "managed_signal_supervision_required")
				if checkManagedSignalHealth(ctx, m) != nil {
					add("signal", "verification_health", "blocked", "signal_verification_endpoint_unavailable_or_changed")
				} else {
					add("signal", "verification_health", "satisfied", "verification_health_grants_no_runtime_authority")
				}
			} else {
				service, e := inspectSignalServiceRetained(binding, false)
				if e == nil {
					e = signalManager.Verify(ctx, service)
				}
				if e != nil {
					add("signal", "supervision", "blocked", "signal_service_or_health_unavailable_or_changed")
				} else {
					add("signal", "supervision", "satisfied", "managed_verifier_current_no_runtime_authority")
				}
			}
		}
		add("signal", "activation", "blocked", "signal_verifier_to_runtime_transition_required")
	}
	if c.Worker.Interaction != nil {
		for _, destination := range c.Worker.Interaction.Destinations {
			configured := destination.Provider == "telegram" && c.Telegram != nil && destination.Account == c.Telegram.Account || destination.Provider == "signal" && c.Signal != nil && destination.Account == c.Signal.Account
			state, reason := "satisfied", "conversation_adapter_configured"
			if !configured {
				state, reason = "blocked", "conversation_adapter_missing"
			}
			add("conversation/"+destination.ID, "adapter", state, reason)
		}
		for _, w := range c.Worker.Interaction.Workers {
			for name, command := range w.Commands {
				state, reason := "satisfied", "read_descriptor_only_qualification_still_required"
				if command.Effect != "read" {
					state, reason = "blocked", "production_mutation_admission_not_implemented"
				}
				add("command/"+w.Producer.ID+"/"+name, "effect", state, reason)
			}
		}
		add("commands", "activation", "blocked", "managed_command_activation_not_implemented")
	}
	if c.LEDSocket != "" {
		add("led", "activation", "blocked", "qualified_led_ownership_activation_required")
	}
	qualification()
	if drained {
		current, e := inspectManagedService(ctx, services, m, false)
		if e != nil || current.Active != "inactive" && current.Active != "failed" {
			return ActivationReview{}, fmt.Errorf("gateway restarted during activation review")
		}
	}
	// Refuse a copied/replaced coordination path or changed revision rather than
	// returning an observation apparently bound to the old installation.
	if err = checkManagedDirectoryPath(d, root, true); err != nil {
		return ActivationReview{}, err
	}
	if err = managedLeaseMatches(d, "operation.lock", operation); err != nil {
		return ActivationReview{}, err
	}
	if installation != nil {
		if err = managedLeaseMatches(d, "installation.lock", installation); err != nil {
			return ActivationReview{}, err
		}
	}
	current, _, err := qualificationInstallation(d, root)
	if err != nil || protocol.Hash(current) != protocol.Hash(m) {
		return ActivationReview{}, fmt.Errorf("installation changed during activation review")
	}
	if err = ctx.Err(); err != nil {
		return ActivationReview{}, err
	}
	out.CheckedAt = now().UTC()
	slices.SortFunc(out.Checks, func(a, b worker.AdmissionCheck) int { return strings.Compare(a.Scope+"/"+a.Stage, b.Scope+"/"+b.Stage) })
	return out, nil
}

func runActivationReviewCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker activation-review", flag.ContinueOnError)
	local := f.Bool("local", false, "Review this user's managed verification installation")
	root := f.String("root", "", "Existing managed gateway installation root")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || !managedPath(*root) || f.NArg() != 0 {
		return fmt.Errorf("activation-review requires --local and a clean absolute --root")
	}
	if ui.DryRun {
		_, err := fmt.Fprintln(out, "[DRY] activation review inputs validated; installation, manager, endpoints and worker state checks deferred")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	review, err := ReviewActivation(ctx, *root)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(review)
}
