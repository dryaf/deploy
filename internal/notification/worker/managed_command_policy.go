package worker

import (
	"encoding/json"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Command policy is an immutable runtime overlay. Canonical recovery and owner
// configuration hashes remain unchanged. Every new policy can only remove rights;
// enrollment overlays also preserve immutable audiences and worker contracts.
func (s *Store) commandConfig() *interaction.Config {
	if s.managedCommandPolicy != nil {
		return s.managedCommandPolicy
	}
	return s.config.Interaction
}

func decodeCommandPolicy(raw string) ([]interaction.Permission, error) {
	var permissions []interaction.Permission
	if raw == "" || len(raw) > 16384 || protocol.Decode([]byte(raw), &permissions, 16384) != nil || permissions == nil {
		return nil, fmt.Errorf("command policy must be a JSON permission array of at most 16384 bytes")
	}
	canonical, err := json.Marshal(permissions)
	if err != nil || string(canonical) != raw {
		return nil, fmt.Errorf("noncanonical command policy")
	}
	return permissions, nil
}
func validManagedCommandPolicy(raw string) bool {
	_, err := decodeCommandPolicy(raw)
	if err == nil {
		return true
	}
	_, err = decodeCommandEnrollment(raw)
	return err == nil
}

func permissionsSubset(before, after []interaction.Permission) bool {
	c := interaction.Config{Permissions: before}
	for _, p := range after {
		for _, permission := range p.Permissions {
			scope, ok := c.Authorize(p.Actor, p.Conversation, p.Worker, permission)
			if !ok || !scope.Allows(p.Scope) {
				return false
			}
		}
	}
	return true
}
func policySubset(before, after string) bool {
	if next, err := decodeCommandEnrollment(after); err == nil {
		if before == "" {
			return true
		}
		if prior, err := decodeCommandEnrollment(before); err == nil {
			return enrollmentSubset(prior, next)
		}
		p, err := decodeCommandPolicy(before)
		return err == nil && permissionsSubset(p, next.Permissions)
	}
	if _, err := decodeCommandEnrollment(before); err == nil {
		return false
	}
	a, err := decodeCommandPolicy(after)
	if err != nil {
		return false
	}
	if before == "" {
		return true
	}
	b, err := decodeCommandPolicy(before)
	return err == nil && permissionsSubset(b, a)
}
func commandPolicyConfig(base *interaction.Config, raw string) (*interaction.Config, error) {
	if base == nil {
		return nil, fmt.Errorf("interaction policy unavailable")
	}
	next := *base
	if raw == "" {
		return &next, nil
	}
	if c, err := decodeCommandEnrollment(raw); err == nil {
		if !enrollmentSubset(base, c) {
			return nil, fmt.Errorf("enrollment exceeds canonical authority")
		}
		return c, nil
	}
	p, err := decodeCommandPolicy(raw)
	if err != nil {
		return nil, err
	}
	next.Permissions = p
	if next.Validate() != nil || !permissionsSubset(base.Permissions, p) {
		return nil, fmt.Errorf("retained policy exceeds canonical authority")
	}
	return &next, nil
}
func narrowedCommandConfig(base *interaction.Config, before, after string) (*interaction.Config, error) {
	current, err := commandPolicyConfig(base, before)
	if err != nil {
		return nil, err
	}
	next, err := commandPolicyConfig(base, after)
	if err != nil {
		return nil, err
	}
	if !policySubset(before, after) || !enrollmentSubset(current, next) {
		return nil, fmt.Errorf("command policy may only narrow existing enrollment, permissions and scope")
	}
	return next, nil
}

// CommitManagedCommandPolicy retains narrowing in the same independently anchored
// history as enable/disable. Re-enabling or recovery cannot restore removed rights.
func (s *Store) CommitManagedCommandPolicy(hash, previous, policy string) error {
	if policy == "" {
		return fmt.Errorf("explicit command policy required")
	}
	return s.commitManagedCommandPolicy("drain", hash, previous, policy, nil)
}

// ValidateManagedCommandPolicyFile is a read-only preflight under the installer's
// operation lock. Commit independently validates both SQL and file authority after
// stopping the runtime; this preflight never opens or repairs the live database.
func ValidateManagedCommandPolicyFile(path string, base *interaction.Config, previous, policy string) error {
	root, err := safefs.Open(path)
	if err != nil {
		return err
	}
	defer root.Close()
	d, err := root.Sub("managed-commands")
	if err != nil {
		return err
	}
	defer d.Close()
	head, err := commandFileHead(d)
	if err != nil {
		return err
	}
	if head == nil || head.Hash != previous || head.Mode != "drain" {
		return fmt.Errorf("disable commands before narrowing permissions")
	}
	if err = verifyCommandHistory(d, head, ""); err != nil {
		return err
	}
	return validateCommandEnrollmentHistory(d, head, base, policy)
}
