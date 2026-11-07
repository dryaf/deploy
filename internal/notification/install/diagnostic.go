package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

type DiagnosticResult struct {
	Version   int               `json:"version"`
	Operation string            `json:"operation"`
	Identity  protocol.Identity `json:"identity"`
	Status    string            `json:"status"`
	Qualified bool              `json:"qualified"`
	Provider  string            `json:"provider,omitempty"`
}
type diagnosticRecord struct {
	Manifest string           `json:"manifest"`
	Provider string           `json:"provider"`
	Result   DiagnosticResult `json:"result"`
}

// TestDeliveryOnce performs one explicitly requested diagnostic per canonical
// installation revision. Pending/ambiguous attempts are never automatically
// repeated, and an accepted provider request is never qualification evidence.
func TestDeliveryOnce(ctx context.Context, root, expected string) (DiagnosticResult, error) {
	return testDeliveryOnce(ctx, root, expected, sendDiagnostic, nil)
}

// TestDeliveryOnceFor selects one configured fixed destination. Selecting the
// ordinary primary shares the historical default diagnostic record, so spelling
// it explicitly cannot bypass an ambiguous or accepted prior attempt.
func TestDeliveryOnceFor(ctx context.Context, root, expected, selected string) (DiagnosticResult, error) {
	return testDeliveryOnceFor(ctx, root, expected, selected, sendDiagnostic, nil)
}

func testDeliveryOnce(ctx context.Context, root, expected string, send func(context.Context, Runtime, ManagedManifest, string, protocol.Message) provider.Result, fault func(string) error) (DiagnosticResult, error) {
	return testDeliveryOnceFor(ctx, root, expected, "", send, fault)
}

func testDeliveryOnceFor(ctx context.Context, root, expected, selected string, send func(context.Context, Runtime, ManagedManifest, string, protocol.Message) provider.Result, fault func(string) error) (DiagnosticResult, error) {
	var zero DiagnosticResult
	if !safefs.DigestPattern.MatchString(expected) || send == nil {
		return zero, fmt.Errorf("exact installation receipt required for test")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return zero, err
	}
	defer d.Close()
	operation, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return zero, err
	}
	defer operation.Close()
	lease, err := d.Lock("installation.lock", false, false)
	if err != nil {
		return zero, err
	}
	defer lease.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return zero, err
	}
	m := j.Intent.After
	if j.Phase != "complete" || m.Mode == "uninstalled" || m.Root != root || protocol.Hash(m) != expected {
		return zero, fmt.Errorf("test requires the exact completed installation")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return zero, err
	}
	c, err := verifyManagedArtifacts(d, m)
	if err != nil {
		return zero, err
	}
	primary, primaryErr := diagnosticProvider(c, m)
	if selected == "" {
		if primaryErr != nil {
			return zero, primaryErr
		}
		selected = primary
	}
	if err = validateDiagnosticProvider(c, m, selected); err != nil {
		return zero, err
	}
	if err = d.Mkdir("diagnostics"); err != nil {
		return zero, err
	}
	diagnostics, err := d.Sub("diagnostics")
	if err != nil {
		return zero, err
	}
	defer diagnostics.Close()
	key := expected
	if primaryErr != nil || selected != primary {
		key += "-" + protocol.Hash(selected)
	}
	name := key + ".json"
	lockName := key + ".lock"
	claimed := false
	if f, e := diagnostics.Open(lockName, os.O_RDONLY, 0); e == nil {
		claimed = true
		f.Close()
	} else if !errors.Is(e, os.ErrNotExist) {
		return zero, e
	}
	if !claimed {
		if f, e := diagnostics.Open(name, os.O_RDONLY, 0); e == nil {
			f.Close()
			return zero, fmt.Errorf("diagnostic lock missing; review retained delivery outcome")
		} else if !errors.Is(e, os.ErrNotExist) {
			return zero, e
		}
	}
	lock, err := diagnostics.Lock(lockName, true, true)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	var record diagnosticRecord
	err = diagnostics.ReadJSON(name, &record)
	if err == nil {
		if !claimed || record.Manifest != expected || record.Provider != selected || record.Result.Version != 1 || !safefs.ValidID(record.Result.Operation, "n") || record.Result.Identity != m.Identity || record.Result.Qualified {
			return zero, fmt.Errorf("diagnostic authority changed")
		}
		if record.Result.Provider != "" && record.Result.Provider != selected {
			return zero, fmt.Errorf("diagnostic provider changed")
		}
		record.Result.Provider = selected
		switch record.Result.Status {
		case "prepared": // Intent was durable, but sending had not been authorized yet.
		case "sending":
			record.Result.Status = "delivery_unknown"
			return record.Result, nil
		case "provider_accepted", "not_accepted", "delivery_unknown":
			return record.Result, nil
		default:
			return zero, fmt.Errorf("invalid diagnostic outcome")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if claimed {
			return zero, fmt.Errorf("diagnostic record missing; delivery outcome requires review")
		}
		id, e := safefs.NewID("n")
		if e != nil {
			return zero, e
		}
		record = diagnosticRecord{Manifest: expected, Provider: selected, Result: DiagnosticResult{Version: 1, Operation: id, Identity: m.Identity, Status: "prepared", Qualified: false, Provider: selected}}
		if err = diagnostics.WriteJSON(name, record, false); err != nil {
			return zero, err
		}
	} else {
		return zero, err
	}
	boundary := func(stage string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fault != nil {
			return fault(stage)
		}
		return nil
	}
	if err = boundary("prepared"); err != nil {
		return record.Result, err
	}
	record.Result.Status = "sending"
	if err = diagnostics.WriteJSON(name, record, true); err != nil {
		return record.Result, err
	}
	if err = boundary("sending"); err != nil {
		return record.Result, err
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sent := send(bounded, c, m, selected, protocol.Message{ID: record.Result.Operation, Producer: protocol.Producer{ID: "diagnostic", Epoch: record.Result.Operation}, Text: "Explicit notification-worker diagnostic. This does not qualify backup delivery."})
	if err = boundary("sent"); err != nil {
		return record.Result, err
	}
	record.Result.Status = "delivery_unknown"
	if sent.Accepted {
		record.Result.Status = "provider_accepted"
	} else if sent.Permanent && !sent.Ambiguous {
		record.Result.Status = "not_accepted"
	}
	if err = diagnostics.WriteJSON(name, record, true); err != nil {
		return record.Result, err
	}
	return record.Result, boundary("complete")
}

// One explicit diagnostic targets the declared primary, never silently sends to
// both destinations or falls back after an ambiguous result.
func diagnosticProvider(c Runtime, m ManagedManifest) (string, error) {
	selected := ""
	if len(c.Providers) == 1 {
		for name := range c.Providers {
			selected = name
		}
	} else if route, ok := c.Worker.Routes["alerts"]; ok {
		selected = route.Primary
	}
	return selected, validateDiagnosticProvider(c, m, selected)
}

func validateDiagnosticProvider(c Runtime, m ManagedManifest, selected string) error {
	p, ok := c.Providers[selected]
	if !ok || p.Conversation != nil {
		return fmt.Errorf("diagnostic requires a configured fixed destination; select a provider explicitly if the alerts primary is ambiguous")
	}
	if p.Kind != "telegram" && !(p.Kind == "signal" && m.FreshSignal != nil) {
		return fmt.Errorf("diagnostic requires Telegram or an owned fresh Signal service")
	}
	return nil
}

func sendDiagnostic(ctx context.Context, c Runtime, m ManagedManifest, name string, message protocol.Message) provider.Result {
	adapter := provider.Adapter{Profiles: c.Providers}
	if c.Providers[name].Kind == "signal" {
		check := func(ctx context.Context) error { return checkFreshSignalGatewayHealth(ctx, m, 0) }
		if err := check(ctx); err != nil {
			return provider.Result{Permanent: true}
		}
		transport, closeTransport, err := managedSignalTransport(m, check, func(error) {})
		if err != nil {
			return provider.Result{Permanent: true}
		}
		defer closeTransport()
		adapter.SignalTransport = transport
	}
	return adapter.Send(ctx, name, message)
}
