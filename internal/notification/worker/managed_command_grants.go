package worker

import (
	"fmt"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"os"
)

// Explicit grants restore only authority admitted by the original receiver.
// Workers, descriptors, aliases and conversation audiences remain unchanged.
// Ordinary narrowing, enable and recovery can never restore removed rights.
func changedCommandConfig(base *interaction.Config, before, after string, grant bool) (*interaction.Config, error) {
	if !grant {
		return narrowedCommandConfig(base, before, after)
	}
	current, err := commandPolicyConfig(base, before)
	if err != nil {
		return nil, err
	}
	next, err := commandPolicyConfig(base, after)
	if err != nil {
		return nil, err
	}
	if commandPolicyVersion(after) != 3 || before == after || permissionsSubset(current.Permissions, next.Permissions) {
		return nil, fmt.Errorf("explicit grant requires restored permissions within original authority")
	}
	same := *next
	same.Permissions, same.Revision = current.Permissions, current.Revision
	if protocol.Hash(same) != protocol.Hash(current) {
		return nil, fmt.Errorf("restore command permissions separately from enrollment changes")
	}
	return next, nil
}

func (s *Store) CommitManagedCommandGrant(hash, previous, policy string) error {
	return s.commitManagedCommandPolicy("drain", hash, previous, policy, nil, true)
}

func ValidateManagedCommandGrantFile(path string, base *interaction.Config, previous, policy string) error {
	return validateManagedCommandPolicyFile(path, base, previous, policy, true)
}

// ManagedCommandGrantsRequired detects retained restoration history before a
// replacement helper can stop the service. Runtime opening independently checks
// completion and SQL authority; pending adjacent heads are allowed by this probe.
func ManagedCommandGrantsRequired(path string) (bool, error) {
	root, err := safefs.Open(path)
	if err != nil {
		return false, err
	}
	defer root.Close()
	d, err := root.Sub("managed-commands")
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer d.Close()
	head, err := commandFileHead(d)
	if err != nil {
		return false, err
	}
	found := false
	for depth := 0; head != nil; depth++ {
		if depth >= 1024 {
			return false, fmt.Errorf("command history capability review exceeds bound")
		}
		var transition managedCommandTransition
		if err = d.ReadJSON(head.Hash+".json", &transition); err != nil {
			return false, err
		}
		if transition.After != *head || !validCommandTransition(transition) {
			return false, fmt.Errorf("command history capability review changed")
		}
		found = found || transition.Grant
		head = transition.Before
	}
	return found, nil
}
