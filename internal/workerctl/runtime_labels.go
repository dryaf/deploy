package workerctl

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// A route revision is generated history, not desired policy. Keep an existing
// label only when YAML still selects exactly the same provider sequence.
func preserveRuntimeRouteLabels(before install.RuntimeInput, after *install.RuntimeInput) {
	after.Worker.Routes = maps.Clone(after.Worker.Routes)
	for name, route := range after.Worker.Routes {
		if old, ok := before.Worker.Routes[name]; ok && old.Primary == route.Primary && old.Fallback == route.Fallback {
			route.Revision = old.Revision
			after.Worker.Routes[name] = route
		}
	}
}

// A successfully adopted runtime may predate the coordinator's serialization.
// Its exact private digest remains authoritative; semantic comparison permits
// whitespace differences without rewriting installed or public runtime bytes.
func retainedNotificationInput(paths ResolvedPaths, input *install.RuntimeInput) ([]byte, bool, error) {
	d, err := safefs.Open(paths.Control)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer d.Close()
	// Only explicit adoption retains legacy serialization. Ordinary bootstrap
	// can legitimately have input pins before publishing its first runtime.
	var adoption notificationAdoptionIntent
	if err = d.ReadJSON("adoption.json", &adoption); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	if err = validateAdoptionIntent(adoption); err != nil {
		return nil, false, err
	}
	var pins notificationInputPins
	if err = d.ReadJSON("notification-inputs.json", &pins); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	raw, err := readLimitsRuntime(paths.Runtime)
	if err != nil || inputDigest(raw) != pins.Runtime {
		return nil, false, fmt.Errorf("retained runtime unavailable or changed")
	}
	var before install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &before, maxControlFrame); err != nil {
		return nil, false, err
	}
	preserveRuntimeRouteLabels(before, input)
	if protocol.Hash(before) != protocol.Hash(*input) {
		return nil, false, fmt.Errorf("runtime differs from declared policy; explicit migration required")
	}
	return bytes.Clone(raw), true, nil
}

func (s *notificationBootstrapStep) inputBytes() ([]byte, error) {
	raw, retained, err := retainedNotificationInput(s.paths, &s.input)
	if err != nil || retained {
		return raw, err
	}
	return s.input.Bytes()
}
