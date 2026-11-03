package install

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const qualificationWindow = 30 * 24 * time.Hour
const maxQualificationChecks = 1024

// These are operator attestations with artifact digests, not automatically
// executed tests. Even a complete report cannot enable production or mutations.
type QualificationBinding struct {
	Manifest     ManagedManifest   `json:"manifest"`
	Architecture string            `json:"architecture"`
	UID          int               `json:"uid"`
	Inputs       map[string]string `json:"input_hashes"`
}

type QualificationCheck struct {
	ID           string    `json:"id"`
	Scope        string    `json:"scope"`
	Result       string    `json:"result"` // pending, passed, failed
	ObservedAt   time.Time `json:"observed_at,omitempty"`
	EvidenceHash string    `json:"evidence_sha256,omitempty"`
}

type QualificationEnvironment struct {
	SignalImageDigest string `json:"signal_image_digest,omitempty"`
	LEDVersion        string `json:"led_version,omitempty"`
	LEDConfigHash     string `json:"led_config_sha256,omitempty"`
}

type QualificationReport struct {
	Version     int                      `json:"version"`
	ID          string                   `json:"id"`
	Binding     QualificationBinding     `json:"binding"`
	CreatedAt   time.Time                `json:"created_at"`
	Operator    string                   `json:"operator"`
	Environment QualificationEnvironment `json:"environment"`
	Checks      []QualificationCheck     `json:"checks"`
}

func qualificationChecks(c Runtime) []QualificationCheck {
	var checks []QualificationCheck
	add := func(scope string, ids ...string) {
		for _, id := range ids {
			checks = append(checks, QualificationCheck{ID: id, Scope: scope, Result: "pending"})
		}
	}
	add("installation", "automated-release", "native-runtime", "systemd-restart", "reboot-recovery", "power-loss", "external-monitor")
	for name := range c.Providers {
		add("provider/"+name, "destination-receipt", "payload-boundaries", "delivery-recovery")
	}
	for name, route := range c.Worker.Routes {
		add("route/"+name, "outage-catchup")
		if route.Fallback != "" {
			add("route/"+name, "fallback-receipt", "both-providers-fail")
		}
	}
	for _, e := range c.Worker.Producers {
		add("producer/"+e.Producer.ID, "generation-cutover", "receipt-reconciliation", "report-monitoring")
	}
	if qualificationUsesSignal(c) {
		add("signal", "account-continuity", "relay-contention", "receive-maintenance")
	}
	if c.LEDSocket != "" {
		add("led", "visual-patterns", "healthy-ttl-expiry", "daemon-restart", "socket-loss", "persistence-failure", "owned-state-cleanup")
	}
	if c.Worker.Interaction != nil {
		for _, d := range c.Worker.Interaction.Destinations {
			add("conversation/"+d.ID, "reply-identity", "sender-authorization", "replay-cutoff")
		}
		for _, w := range c.Worker.Interaction.Workers {
			add("commands/"+w.Producer.ID, "read-routing", "drain-recovery")
			for name, command := range w.Commands {
				if command.Effect != "read" {
					add("command/"+w.Producer.ID+"/"+name, "confirmation", "idempotency", "authorization-expiry")
				}
			}
		}
	}
	slices.SortFunc(checks, func(a, b QualificationCheck) int { return strings.Compare(a.Scope+"/"+a.ID, b.Scope+"/"+b.ID) })
	return checks
}

func qualificationUsesSignal(c Runtime) bool {
	if c.Signal != nil {
		return true
	}
	for _, p := range c.Providers {
		if p.Kind == "signal" {
			return true
		}
	}
	return false
}

func qualificationBinding(m ManagedManifest, c Runtime) (b QualificationBinding, err error) {
	b = QualificationBinding{Manifest: m, Architecture: runtime.GOARCH, UID: os.Getuid(), Inputs: map[string]string{}}
	paths := map[string]string{}
	for name, p := range c.Providers {
		if p.TokenFile != "" {
			paths["provider/"+name+"/token"] = p.TokenFile
		}
		if p.ChatFile != "" {
			paths["provider/"+name+"/chat"] = p.ChatFile
		}
	}
	if c.Telegram != nil {
		paths["interactive/telegram/token"] = c.Telegram.TokenFile
	}
	for key, path := range paths {
		data, e := readManagedInput(path, 8192)
		if e != nil {
			return b, fmt.Errorf("qualification input %s unavailable: %w", key, e)
		}
		if len(data) == 0 {
			return b, fmt.Errorf("empty qualification input %s", key)
		}
		b.Inputs[key] = managedDigest(data)
	}
	return b, nil
}

func (r QualificationReport) validate() error {
	b := r.Binding
	if r.Version != 1 || !safefs.ValidID(r.ID, "n") || b.Manifest.validate() != nil || b.Manifest.Mode != "verification" || (b.Architecture != "amd64" && b.Architecture != "arm64") || b.UID < 0 || b.Inputs == nil || len(b.Inputs) > 257 || r.CreatedAt.IsZero() || !protocol.ValidToken(r.Operator) || len(r.Checks) == 0 || len(r.Checks) > maxQualificationChecks {
		return fmt.Errorf("invalid qualification report")
	}
	for key, hash := range b.Inputs {
		if len(key) > 256 || key == "" || !safefs.DigestPattern.MatchString(hash) {
			return fmt.Errorf("invalid qualification input digest")
		}
	}
	e := r.Environment
	if (e.SignalImageDigest != "" && (!strings.HasPrefix(e.SignalImageDigest, "sha256:") || !safefs.DigestPattern.MatchString(strings.TrimPrefix(e.SignalImageDigest, "sha256:")))) || (e.LEDVersion != "" && !protocol.ValidToken(e.LEDVersion)) || (e.LEDConfigHash != "" && !safefs.DigestPattern.MatchString(e.LEDConfigHash)) {
		return fmt.Errorf("invalid qualification environment")
	}
	previous := ""
	for _, c := range r.Checks {
		key := c.Scope + "/" + c.ID
		if !protocol.ValidToken(c.ID) || c.Scope == "" || len(c.Scope) > 512 || key <= previous {
			return fmt.Errorf("duplicate, unordered or invalid qualification check")
		}
		previous = key
		switch c.Result {
		case "pending":
			if !c.ObservedAt.IsZero() || c.EvidenceHash != "" {
				return fmt.Errorf("pending check carries completed evidence")
			}
		case "passed", "failed":
			if c.ObservedAt.Before(r.CreatedAt) || !c.ObservedAt.Before(r.CreatedAt.Add(qualificationWindow)) || !safefs.DigestPattern.MatchString(c.EvidenceHash) {
				return fmt.Errorf("qualification evidence outside report window or missing digest")
			}
		default:
			return fmt.Errorf("invalid qualification result")
		}
	}
	return nil
}

func (r QualificationReport) matches(b QualificationBinding, c Runtime, now time.Time) error {
	if err := r.validate(); err != nil {
		return err
	}
	if protocol.Hash(b) != protocol.Hash(r.Binding) || now.Before(r.CreatedAt) || !now.Before(r.CreatedAt.Add(qualificationWindow)) {
		return fmt.Errorf("qualification binding changed or report expired")
	}
	want := qualificationChecks(c)
	if len(want) != len(r.Checks) {
		return fmt.Errorf("qualification check coverage changed")
	}
	for i, check := range r.Checks {
		if check.ID != want[i].ID || check.Scope != want[i].Scope || check.ObservedAt.After(now) {
			return fmt.Errorf("qualification check coverage or observation time invalid")
		}
	}
	// Unspecified environment leaves reports incomplete; inventing irrelevant
	// environment identities is rejected instead of widening their authority.
	if (!qualificationUsesSignal(c) && r.Environment.SignalImageDigest != "") || (c.LEDSocket == "" && (r.Environment.LEDVersion != "" || r.Environment.LEDConfigHash != "")) {
		return fmt.Errorf("qualification environment is not configured")
	}
	return nil
}

func (r QualificationReport) evidenceComplete(c Runtime) bool {
	if qualificationUsesSignal(c) && r.Environment.SignalImageDigest == "" {
		return false
	}
	if c.LEDSocket != "" && (r.Environment.LEDVersion == "" || r.Environment.LEDConfigHash == "") {
		return false
	}
	return len(r.Checks) > 0 && !slices.ContainsFunc(r.Checks, func(c QualificationCheck) bool { return c.Result != "passed" })
}
