package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func mappingRegistry(ctx context.Context, api LEDAPI) ([]led.Entry, error) {
	entries, err := api.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(entries) > 1024 {
		return nil, fmt.Errorf("LED registry exceeds budget")
	}
	seen := map[string]bool{}
	for _, e := range entries {
		key := fmt.Sprintf("%d:%s", e.OwnerUID, e.ID)
		if e.OwnerUID < 0 || !ledPatternName.MatchString(e.ID) || (e.LED != "green" && e.LED != "red") || seen[key] {
			return nil, fmt.Errorf("invalid LED registry")
		}
		seen[key] = true
	}
	return entries, nil
}

func preflightLEDMapping(ctx context.Context, api LEDAPI, v ledMappingIntent, initial bool) error {
	status, err := api.Status(ctx)
	if err != nil {
		return err
	}
	if status.Degraded {
		return fmt.Errorf("LED daemon degraded")
	}
	for _, b := range ledLayoutOutputs(v.After) {
		var target struct {
			Enabled, Available bool
			DefaultPattern     string `json:"default_pattern"`
		}
		if json.Unmarshal(status.LEDs[b.LED], &target) != nil || !target.Enabled || !target.Available || (target.DefaultPattern != "off" && target.DefaultPattern != "blink-slow") {
			return fmt.Errorf("LED target unavailable or default does not indicate unknown")
		}
		for _, style := range b.Styles {
			if _, ok := status.Patterns[style.Pattern]; !ok {
				return fmt.Errorf("LED migration pattern unavailable")
			}
		}
	}
	if !initial {
		return nil
	}
	entries, err := mappingRegistry(ctx, api)
	if err != nil {
		return err
	}
	owned := map[string]led.State{}
	total := len(entries)
	for _, e := range entries {
		if e.OwnerUID == v.Before.UID {
			owned[e.ID] = e.State
		}
	}
	// A newly enrolled namespace cannot claim an unfamiliar existing state.
	for _, b := range v.After.Bindings {
		if slices.ContainsFunc(v.Before.Bindings, func(old LEDBinding) bool { return old.ID == b.ID }) {
			continue
		}
		base := ledBindingNamespace(v.After.Worker, b.ID)
		_, baseline := owned[base+".baseline"]
		_, healthy := owned[base+".healthy"]
		if baseline || healthy {
			return fmt.Errorf("new LED binding namespace is already occupied")
		}
	}
	// Account for ordered revocation/removal before reserving baseline capacity.
	for _, step := range v.Steps {
		if step.Kind == "clear" {
			if st, ok := owned[step.ID]; ok {
				if st.LED != step.LED {
					return fmt.Errorf("reserved LED state moved to an unexpected target")
				}
				delete(owned, step.ID)
				total--
			}
			continue
		}
		if st, exists := owned[step.ID]; exists {
			if protocol.Hash(st) != protocol.Hash(*step.State) {
				return fmt.Errorf("reserved LED baseline differs from the migration intent")
			}
		} else {
			if len(owned) >= 128 || total >= 1024 {
				return fmt.Errorf("LED migration exceeds registry capacity")
			}
			owned[step.ID] = *step.State
			total++
		}
	}
	return nil
}

func checkLEDMappingStep(ctx context.Context, api LEDAPI, step ledMappingStep, uid int) error {
	entries, err := mappingRegistry(ctx, api)
	if err != nil {
		return err
	}
	owned, exists := 0, false
	for _, e := range entries {
		if e.OwnerUID != uid {
			continue
		}
		owned++
		if e.ID == step.ID {
			exists = true
			if e.LED != step.LED {
				return fmt.Errorf("reserved LED state moved to an unexpected target")
			}
			if step.Kind == "set" && protocol.Hash(e.State) != protocol.Hash(*step.State) {
				return fmt.Errorf("reserved LED baseline differs from the migration intent")
			}
		}
	}
	if step.Kind == "set" && !exists && (owned >= 128 || len(entries) >= 1024) {
		return fmt.Errorf("LED migration registry capacity unavailable")
	}
	return nil
}

func verifyLEDMappingOutput(ctx context.Context, api LEDAPI, v ledMappingIntent) error {
	if err := preflightLEDMapping(ctx, api, v, false); err != nil {
		return err
	}
	entries, err := mappingRegistry(ctx, api)
	if err != nil {
		return err
	}
	owned := map[string]led.State{}
	for _, e := range entries {
		if e.OwnerUID == v.After.UID {
			owned[e.ID] = e.State
		}
	}
	retained := map[string]bool{}
	for _, b := range ledLayoutOutputs(v.After) {
		if v.LEDOutputDisabled {
			base := ledBindingNamespace(v.After.Worker, b.ID)
			if _, ok := owned[base+".baseline"]; ok {
				return fmt.Errorf("disabled LED baseline remains")
			}
			if _, ok := owned[base+".healthy"]; ok {
				return fmt.Errorf("disabled LED healthy lease remains")
			}
			continue
		}
		want := ledBaseline(v.After.Worker, b)
		got, ok := owned[want.ID]
		if !ok || protocol.Hash(got) != protocol.Hash(want) {
			return fmt.Errorf("new LED baseline not confirmed")
		}
		retained[want.ID] = true
		if _, ok := owned[ledBindingNamespace(v.After.Worker, b.ID)+".healthy"]; ok {
			return fmt.Errorf("LED migration still has a healthy lease")
		}
	}
	for _, b := range ledLayoutOutputs(v.Before) {
		if _, ok := owned[ledBindingNamespace(v.Before.Worker, b.ID)+".healthy"]; ok {
			return fmt.Errorf("old LED healthy lease remains")
		}
		baseline := ledBindingNamespace(v.Before.Worker, b.ID) + ".baseline"
		if _, ok := owned[baseline]; ok && !retained[baseline] {
			return fmt.Errorf("removed LED baseline remains")
		}
	}
	return nil
}
