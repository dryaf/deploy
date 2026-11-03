package worker

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

const workerLEDBinding = "$worker"

// LEDBinding selects one enrolled producer. A sole "*" selects its current
// bounded subject inventory; exact selectors remain unknown when missing.
type LEDBinding struct {
	ID         string              `json:"id"`
	Producer   protocol.Producer   `json:"producer"`
	Subjects   []string            `json:"subjects"`
	LED        string              `json:"led"`
	AllowPower bool                `json:"allow_power,omitempty"`
	Styles     map[string]LEDStyle `json:"styles,omitempty"`
}

type LEDStyle struct {
	Pattern  string `json:"pattern"`
	Priority int    `json:"priority"`
}

type LEDBindingHealth struct {
	Condition       string    `json:"condition"`
	ValidUntil      time.Time `json:"valid_until"`
	Subjects        []string  `json:"subjects"`
	OmittedSubjects int       `json:"omitted_subjects,omitempty"`
	Overflow        bool      `json:"overflow,omitempty"`
	evidence        []protocol.Subject
}

var ledPatternName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

func standardLEDStyles() map[string]LEDStyle {
	return map[string]LEDStyle{
		"baseline": {"blink-slow", 5}, "ok": {"heartbeat", 10},
		"paused": {"blink-1", 40}, "warning": {"blink-2", 60},
		"unknown": {"blink-slow", 80}, "critical": {"blink-fast", 90},
	}
}

func (b LEDBinding) normalized() LEDBinding {
	b.Subjects = slices.Clone(b.Subjects)
	slices.Sort(b.Subjects)
	styles := standardLEDStyles()
	for condition, style := range b.Styles {
		styles[condition] = style
	}
	b.Styles = styles
	return b
}

func (c Config) validateLEDBindings() error {
	if len(c.LEDBindings) > 32 {
		return fmt.Errorf("at most 32 LED bindings are supported")
	}
	seen, total := map[string]bool{}, 0
	bands := map[string][2]int{"baseline": {0, 9}, "ok": {10, 39}, "paused": {40, 59}, "warning": {60, 79}, "unknown": {80, 89}, "critical": {90, 100}}
	for _, b := range c.LEDBindings {
		if !protocol.ValidToken(b.ID) || seen[b.ID] || (b.LED != "green" && b.LED != "red") || (b.LED == "red" && !b.AllowPower) || (b.LED != "red" && b.AllowPower) {
			return fmt.Errorf("invalid LED binding identity or power LED opt-in")
		}
		seen[b.ID] = true
		if _, err := c.enrollment(b.Producer); err != nil {
			return fmt.Errorf("LED binding producer is not enrolled")
		}
		if len(b.Subjects) == 0 || len(b.Subjects) > protocol.MaxSubjects {
			return fmt.Errorf("invalid LED subject selection")
		}
		subjects := map[string]bool{}
		for _, key := range b.Subjects {
			if subjects[key] || (key == "*" && len(b.Subjects) != 1) || (key != "*" && !protocol.ValidToken(key)) {
				return fmt.Errorf("invalid or duplicate LED subject selector")
			}
			subjects[key] = true
		}
		total += len(b.Subjects)
		for condition, style := range b.Styles {
			band, ok := bands[condition]
			if !ok || !ledPatternName.MatchString(style.Pattern) || style.Priority < band[0] || style.Priority > band[1] {
				return fmt.Errorf("invalid LED style or priority band")
			}
		}
	}
	if total > 512 {
		return fmt.Errorf("LED subject selections exceed the configuration budget")
	}
	body, err := json.Marshal(desiredLEDLayout(c, "worker", 0))
	if err != nil || len(body) > protocol.MaxRequest-1024 {
		return fmt.Errorf("LED mapping exceeds storage budget")
	}
	return nil
}

func selectLEDBinding(b LEDBinding, subjects []protocol.Subject, now time.Time) LEDBindingHealth {
	h := LEDBindingHealth{Condition: "retired"}
	selected := subjects
	if b.Subjects[0] == "*" {
		// Retired history is not active inventory. Keep it when everything is
		// retired so explicit retirement can resolve the aggregate's old fault.
		var active []protocol.Subject
		for _, sub := range subjects {
			if sub.Condition != "retired" {
				active = append(active, sub)
			}
		}
		if len(active) > 0 {
			selected = active
		}
	}
	if b.Subjects[0] != "*" {
		selected = nil
		byKey := map[string]protocol.Subject{}
		for _, sub := range subjects {
			byKey[sub.Key] = sub
		}
		for _, key := range b.Subjects {
			sub, exists := byKey[key]
			if !exists {
				sub = protocol.Subject{Key: key, Condition: "unknown", Reason: "selected subject missing"}
			}
			selected = append(selected, sub)
		}
	}
	for _, sub := range selected {
		if !sub.ValidUntil.After(now) && sub.Condition != "critical" {
			sub.Condition = "unknown"
		}
		if rank(sub.Condition) > rank(h.Condition) {
			h.Condition = sub.Condition
		}
		if h.ValidUntil.IsZero() || sub.ValidUntil.Before(h.ValidUntil) {
			h.ValidUntil = sub.ValidUntil
		}
		if len(h.evidence) < protocol.MaxSubjects {
			h.evidence = append(h.evidence, sub)
		}
	}
	if len(selected) == 0 {
		h.Condition = "unknown"
	}
	if len(selected) > protocol.MaxSubjects {
		h.Overflow = true
		if h.Condition != "critical" && h.Condition != "retired" {
			h.Condition = "unknown"
		}
	}
	slices.SortFunc(h.evidence, func(a, b protocol.Subject) int {
		if a.Key < b.Key {
			return -1
		}
		if a.Key > b.Key {
			return 1
		}
		return 0
	})
	// Status already carries the producer's detailed subjects. Bound repeated
	// binding labels independently; omission counts never imply completeness.
	bytes := 0
	for _, sub := range h.evidence {
		if len(h.Subjects) >= 16 || bytes+len(sub.Key) > 512 {
			break
		}
		h.Subjects = append(h.Subjects, sub.Key)
		bytes += len(sub.Key)
	}
	h.OmittedSubjects = len(selected) - len(h.Subjects)
	return h
}
