package workerctl

import (
	"fmt"
	"sort"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
)

func validateSourceTargets(r ControlRequest) error {
	if r.SourceTargets == nil {
		return nil // Earlier private plan callers did not supply inventory endpoints.
	}
	if r.Worker != "backup-worker" || (r.Action != "plan" && r.Action != "status" && r.Action != "run" && r.Action != "activate" && !isQualificationControl(r.Action)) || r.Desired.Backup == nil || len(r.SourceTargets) != len(r.Desired.Backup.Sources) {
		return fmt.Errorf("source targets require a complete backup plan/status or run/qualification/activation check")
	}
	seen := map[string]bool{}
	for _, s := range r.SourceTargets {
		if !config.ValidInventoryName(s.Alias) || !config.HostPattern.MatchString(s.Host) || !config.UserPattern.MatchString(s.User) || s.User == "root" || s.Port < 1 || s.Port > 65535 || seen[s.Alias] {
			return fmt.Errorf("invalid or duplicate backup source plan target")
		}
		seen[s.Alias] = true
	}
	for _, alias := range r.Desired.Backup.Sources {
		if !seen[alias] {
			return fmt.Errorf("backup source plan targets differ from declared aliases")
		}
		delete(seen, alias)
	}
	return nil
}

// Compare only retained aliases. Additions/removals remain the source-set diff,
// and no endpoint value (or private key/trust path) appears in the returned plan.
func sourcePlanChanges(desired, installed []backup.SetupSourceTarget) ([]Change, error) {
	if desired == nil {
		return nil, nil
	}
	if len(installed) == 0 {
		return nil, fmt.Errorf("retained source endpoints unavailable for policy comparison")
	}
	before := map[string]backup.SetupSourceTarget{}
	for _, s := range installed {
		if _, duplicate := before[s.Alias]; duplicate {
			return nil, fmt.Errorf("duplicate retained source alias requires recovery review")
		}
		before[s.Alias] = s
	}
	var changes []Change
	for _, next := range desired {
		old, found := before[next.Alias]
		if !found {
			continue
		}
		for _, field := range []struct {
			name string
			diff bool
		}{{"host", old.Host != next.Host}, {"user", old.User != next.User}, {"ssh_port", old.Port != next.Port}} {
			if field.diff {
				changes = append(changes, Change{Field: "servers." + next.Alias + "." + field.name, Transition: "producer_enrollment"})
			}
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Field < changes[j].Field })
	return changes, nil
}

func declaredSourceTargets(inv config.ServerInventory, d config.DesiredWorkers) ([]backup.SetupSourceTarget, error) {
	var result []backup.SetupSourceTarget
	if d.Backup == nil {
		return nil, fmt.Errorf("backup declaration missing")
	}
	for _, alias := range d.Backup.Sources {
		s, ok := inv.Servers[alias]
		if !ok {
			return nil, fmt.Errorf("declared backup source is absent from inventory")
		}
		port := s.SSHPort
		if port == 0 {
			port = 22
		}
		result = append(result, backup.SetupSourceTarget{Alias: alias, Host: s.Host, User: s.User, Port: port})
	}
	return result, nil
}
