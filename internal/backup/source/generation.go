package source

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/transport"
	"github.com/dryaf/deploy/internal/ui"
)

type SourceGenerationChange struct {
	Schema   int              `json:"schema"`
	SourceID string           `json:"source_id"`
	Service  string           `json:"service"`
	Previous string           `json:"previous"`
	Next     SourceGeneration `json:"next"`
	Reason   string           `json:"reason"`
	At       time.Time        `json:"at"`
}

func reconcileSourceGeneration(home string, request SourceEnrollmentRequest) (SourceGenerationChange, error) {
	var result SourceGenerationChange
	if !safefs.ValidID(request.SourceID, "s") || !config.ValidInventoryName(request.Service) || !safefs.ValidID(request.Previous, "g") || strings.TrimSpace(request.Reason) == "" {
		return result, fmt.Errorf("generation reconciliation requires source UUID, service, previous generation and reason")
	}
	root, err := safefs.Open(home)
	if err != nil {
		return result, err
	}
	defer root.Close()
	dir, err := root.Sub(".local/state/deploy/backup-source")
	if err != nil {
		return result, err
	}
	defer dir.Close()
	// Match the snapshot lock order: export, deployment, then source policy.
	export, err := dir.Lock("export.lock", true, false)
	if err != nil {
		return result, err
	}
	defer export.Close()
	unit, err := safefs.ReadFile(root, ".config/containers/systemd/"+request.Service+".container", protocol.MaxBackupHeader)
	if err != nil {
		return result, err
	}
	m, err := quadlet.ParseMetadata(bytes.NewReader(unit))
	if err != nil || m == nil || m.Service != request.Service || m.Backup == nil || !m.Backup.Enabled {
		return result, fmt.Errorf("selected service has no valid backup declaration")
	}
	locks, err := acquireSourceDeploymentLocks(home, *m)
	if err != nil {
		return result, err
	}
	defer func() {
		for _, lock := range locks {
			lock.Close()
		}
	}()
	release, committed, err := sourceOwnership(home, *m)
	if err != nil {
		return result, err
	}
	if err = sourceEffectiveProfile(home, *m, committed, release); err != nil {
		return result, err
	}
	policy, err := dir.Lock("policy.lock", true, false)
	if err != nil {
		return result, err
	}
	defer policy.Close()
	var identity SourceIdentity
	if err = dir.ReadJSON("identity.json", &identity); err != nil {
		return result, err
	}
	if identity.ID != request.SourceID {
		return result, fmt.Errorf("source identity changed")
	}
	name := "generations/" + safefs.LockName(request.Service) + ".json"
	// The previous-generation keyed intent makes a lost acknowledgement retryable.
	if err = dir.Mkdir("reconciliations"); err != nil {
		return result, err
	}
	intentName := "reconciliations/" + request.Previous + ".json"
	err = dir.ReadJSON(intentName, &result)
	if os.IsNotExist(err) {
		if identity.Generations[request.Service] != request.Previous {
			return result, fmt.Errorf("previous generation differs from source ledger")
		}
		id, e := safefs.NewID("g")
		if e != nil {
			return result, e
		}
		result = SourceGenerationChange{1, identity.ID, request.Service, request.Previous, SourceGeneration{1, id, protocol.MetadataBindingHash(*m), *m}, request.Reason, time.Now().UTC()}
		if err = dir.WriteJSON(intentName, result, false); err != nil {
			return result, err
		}
	} else if err != nil {
		return result, err
	}
	if result.Schema != 1 || result.SourceID != identity.ID || result.Service != request.Service || result.Previous != request.Previous || result.Next.BindingHash != protocol.MetadataBindingHash(*m) || result.Reason != request.Reason {
		return result, fmt.Errorf("generation reconciliation intent conflict")
	}
	current := identity.Generations[request.Service]
	if current != result.Previous && current != result.Next.Generation {
		return result, fmt.Errorf("newer source generation fences this reconciliation")
	}
	if err = dir.WriteJSON(name, result.Next, true); err != nil {
		return result, err
	}
	identity.Generations[request.Service] = result.Next.Generation
	err = dir.WriteJSON("identity.json", identity, true)
	return result, err
}

func ReconcileSourceCLI(args []string) error {
	flags := flag.NewFlagSet("backup-worker source-reconcile", flag.ContinueOnError)
	source := flags.String("source-id", "", "Enrolled source UUID")
	instance := flags.String("instance", "", "Service")
	previous := flags.String("previous", "", "Current enrolled generation")
	reason := flags.String("reason", "", "Reason for a new database generation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || !safefs.ValidID(*source, "s") || !safefs.ValidID(*previous, "g") || !config.ValidInventoryName(*instance) || strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("source-reconcile requires --source-id --instance --previous --reason and a named source server")
	}
	server, err := config.SelectServer(ui.InventoryPath(""), flags.Arg(0))
	if err != nil {
		return err
	}
	if ui.DryRun {
		fmt.Println("[DRY] Reconcile the selected committed source binding; no SSH or writes.")
		return nil
	}
	request := SourceEnrollmentRequest{Schema: 1, Operation: "reconcile-generation", SourceID: *source, Service: *instance, Previous: *previous, Reason: *reason}
	body, _ := json.Marshal(request)
	cmd := transport.WorkerAdminShell(server, `exec "$HOME/.local/lib/deploy/backup-source/deploy" __source_admin`)
	cmd.Stdin = strings.NewReader(string(body))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return process.Run(cmd, 2*time.Minute, 10*time.Second)
}
