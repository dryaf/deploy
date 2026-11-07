package backup

import "errors"

var ErrSourceEndpointMismatch = errors.New("source inventory differs from the installed endpoints; apply the source move before running, qualifying or activating the backup")

// Called under the setup lease with the verified installed runtime. Inventory
// endpoints are outside the worker YAML fingerprint, so lifecycle actions must
// check them separately before starting work or recording observations.
func requireDeclaredSourceTargets(c WorkerInstallation, targets []SetupSourceTarget) error {
	if targets == nil {
		return nil
	} // Earlier specialist/API callers carry no inventory.
	if len(targets) != len(c.Sources) {
		return ErrSourceEndpointMismatch
	}
	seen := map[string]bool{}
	for _, target := range targets {
		if seen[target.Alias] {
			return ErrSourceEndpointMismatch
		}
		seen[target.Alias] = true
		matched := false
		for _, installed := range c.Sources {
			if installed.Alias == target.Alias && installed.Host == target.Host && installed.User == target.User && installed.Port == target.Port {
				matched = true
				break
			}
		}
		if !matched {
			return ErrSourceEndpointMismatch
		}
	}
	return nil
}
