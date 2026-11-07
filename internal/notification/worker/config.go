package worker

import (
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

type Enrollment struct {
	Producer           protocol.Producer `json:"producer"`
	Required           bool              `json:"required"`
	Routes             []string          `json:"routes"`
	MaxSilenceSeconds  int               `json:"max_silence_seconds"`
	MaxEvidenceSeconds int               `json:"max_evidence_seconds"`
	MaxMessages        int               `json:"max_messages"`
	PolicyHealth       bool              `json:"policy_health,omitempty"`
	InteractiveContext bool              `json:"interactive_context,omitempty"`
	ContextKinds       []string          `json:"context_kinds,omitempty"`
}

type Route struct {
	Revision string `json:"revision"`
	Primary  string `json:"primary"`
	Fallback string `json:"fallback,omitempty"`
}

// Config contains resolved identities, never messenger credentials.
type Config struct {
	Producers        []Enrollment        `json:"producers"`
	Routes           map[string]Route    `json:"routes"`
	MaxMessages      int                 `json:"max_messages"`
	MaxPayloadBytes  int64               `json:"max_payload_bytes"`
	MaxDatabaseBytes int64               `json:"max_database_bytes"`
	MinFreeBytes     int64               `json:"min_free_bytes"`
	Interaction      *interaction.Config `json:"interaction,omitempty"`
	LEDBindings      []LEDBinding        `json:"led_bindings,omitempty"`
	ReceiptAuditDays int                 `json:"receipt_audit_days,omitempty"` // Zero uses 90; intermediate receipt history only.
}

func (c Config) Validate() error { return c.validate(false) }

// ValidateBootstrap is only for an explicitly unbound verification gateway.
// Normal runtime validation continues to require at least one real producer.
func (c Config) ValidateBootstrap() error {
	if len(c.Producers) != 0 || c.Interaction != nil || len(c.LEDBindings) != 0 {
		return fmt.Errorf("unbound gateway cannot enroll producers, commands or LED output")
	}
	return c.validate(true)
}

func (c Config) validate(unbound bool) error {
	if c.ReceiptAuditDays != 0 && (c.ReceiptAuditDays < 90 || c.ReceiptAuditDays > 3650) {
		return fmt.Errorf("receipt audit retention must be 90..3650 days")
	}
	if err := c.validateLEDBindings(); err != nil {
		return err
	}
	if c.Interaction != nil {
		if err := c.Interaction.Validate(); err != nil {
			return err
		}
		for _, w := range c.Interaction.Workers {
			if _, err := c.enrollment(w.Producer); err != nil {
				return err
			}
		}
	}
	if !unbound && len(c.Producers) == 0 || len(c.Producers) > 128 || len(c.Routes) > 128 {
		return fmt.Errorf("invalid enrollment size")
	}
	seen := map[string]bool{}
	for name, r := range c.Routes {
		if !protocol.ValidToken(name) || !protocol.ValidToken(r.Revision) || !protocol.ValidToken(r.Primary) || r.Fallback == r.Primary || (r.Fallback != "" && !protocol.ValidToken(r.Fallback)) {
			return fmt.Errorf("invalid route")
		}
	}
	for _, p := range c.Producers {
		if !p.Producer.Valid() || seen[p.Producer.ID] || p.MaxSilenceSeconds < 10 || p.MaxSilenceSeconds > 86400 || p.MaxEvidenceSeconds < 1 || p.MaxEvidenceSeconds > 31*86400 || p.MaxMessages < 1 || p.MaxMessages > 10000 {
			return fmt.Errorf("invalid enrollment")
		}
		seen[p.Producer.ID] = true
		if p.InteractiveContext != (len(p.ContextKinds) > 0) || len(p.ContextKinds) > 16 {
			return fmt.Errorf("interactive target kinds require explicit enrollment")
		}
		kinds := map[string]bool{}
		for _, kind := range p.ContextKinds {
			if !protocol.ValidToken(kind) || kinds[kind] {
				return fmt.Errorf("invalid interactive target kind")
			}
			kinds[kind] = true
		}
		routes := map[string]bool{}
		for _, r := range p.Routes {
			if _, ok := c.Routes[r]; !ok || routes[r] {
				return fmt.Errorf("invalid enrolled route")
			}
			routes[r] = true
		}
	}
	if c.MaxMessages < 1 || c.MaxMessages > 10000 || c.MaxPayloadBytes < 1024 || c.MaxPayloadBytes > 64<<20 || c.MaxDatabaseBytes < 1<<20 || c.MinFreeBytes < 0 {
		return fmt.Errorf("invalid storage budgets")
	}
	return nil
}

func (c Config) enrollment(p protocol.Producer) (Enrollment, error) {
	for _, e := range c.Producers {
		if e.Producer == p {
			return e, nil
		}
	}
	return Enrollment{}, fmt.Errorf("producer not enrolled")
}

const attemptBudget = 10 * time.Second

func terminal(state string) bool {
	switch state {
	case "provider_accepted", "cancelled", "superseded", "expired":
		return true
	}
	return false
}
