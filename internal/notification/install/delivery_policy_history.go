package install

import (
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// deliveryPolicyAncestor follows only exact, immutable command/output intents.
// These transitions keep the runtime, service, delivery owner and qualification
// unchanged. It never normalizes an unproved manifest field or opens live SQL.
func deliveryPolicyAncestor(d *safefs.Directory, current ManagedManifest, expected string) (ManagedManifest, error) {
	for depth := 0; depth < 256; depth++ {
		if protocol.Hash(current) == expected {
			return current, nil
		}
		var err error
		current, err = deliveryPolicyPredecessor(d, current)
		if err != nil {
			return ManagedManifest{}, err
		}
	}
	return ManagedManifest{}, fmt.Errorf("delivery policy history exceeds the review bound")
}

func deliveryPolicyPredecessor(d *safefs.Directory, current ManagedManifest) (ManagedManifest, error) {
	var candidates []string
	if current.Commands != nil {
		candidates = append(candidates, current.Commands.Operation)
	}
	if current.LEDOutput != nil {
		candidates = append(candidates, current.LEDOutput.Operation)
	}
	for _, operation := range candidates {
		var intent managedIntent
		if err := d.ReadJSON("operations/"+operation+"/intent.json", &intent); err != nil {
			return ManagedManifest{}, err
		}
		if protocol.Hash(intent.After) != protocol.Hash(current) {
			continue
		}
		j := managedJournal{Intent: intent, Phase: "complete"}
		var err error
		switch intent.Operation {
		case "enable-commands", "disable-commands", "narrow-commands", "migrate-commands", "restore-command-permissions":
			err = validateCommandJournal(j)
		case "set-led-output":
			err = validateLEDOutputJournal(j)
			if intent.After.Mode != "delivery" {
				err = fmt.Errorf("LED continuation left delivery")
			}
		default:
			err = fmt.Errorf("unsupported delivery policy continuation")
		}
		if err != nil {
			return ManagedManifest{}, err
		}
		if intent.Before == nil || intent.ID != operation {
			return ManagedManifest{}, fmt.Errorf("delivery policy predecessor changed")
		}
		return *intent.Before, nil
	}
	return ManagedManifest{}, fmt.Errorf("delivery policy ancestry is missing or changed")
}
