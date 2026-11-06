package backup

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/safefs"
)

type backupSignalMigration struct {
	Adoption  backupSignalAdoption `json:"adoption"`
	Profile   provider.Profile     `json:"profile"`
	Selectors string               `json:"selectors"`
	Reason    string               `json:"reason"`
}

func (s backupCutoverSpec) signalProfile() *provider.Profile {
	if s.Signal == nil {
		return nil
	}
	return &s.Signal.Profile
}

func validateBackupSignalMigration(s backupCutoverSpec, c WorkerInstallation) error {
	if s.Signal == nil || s.Signal.Adoption.validate() != nil || c.Policy.Notifications.Signal == nil || !safefs.DigestPattern.MatchString(s.Signal.Selectors) || s.Signal.Reason == "" || len(s.Signal.Reason) > 512 {
		return fmt.Errorf("Signal migration requires retained legacy account adoption")
	}
	p := s.Signal.Profile
	if p.Kind != "signal" || p.Conversation == nil || p.TokenFile != "" || p.ChatFile != "" || p.Conversation.Provider != "signal" || p.Conversation.Account != s.Signal.Adoption.Spec.Account || p.Conversation.Thread != "" || p.Destination != p.Conversation.Fingerprint() || !backupRecoveryPath(p.Socket) || !p.RelayIdentity.Valid() || !notify.ValidToken(p.Conversation.ID) || !notify.ValidToken(p.Conversation.Generation) {
		return fmt.Errorf("Signal migration requires the exact enrolled conversation and relay")
	}
	return nil
}

// This runs while the managed producer lifecycle holds the gateway leases.
// Adoption evidence proves the old account receiver was permanently fenced.
func requireBackupSignalMigration(ctx context.Context, home string, c WorkerInstallation, s backupCutoverSpec, session install.ManagedProducerSession, revoke bool) error {
	if s.Signal == nil {
		return nil
	}
	a := s.Signal.Adoption
	if a.Spec.Home != home {
		return fmt.Errorf("Signal migration changed installation user")
	}
	d, err := safefs.Open(a.Spec.Inbox)
	if err != nil {
		return err
	}
	defer d.Close()
	var retained backupSignalAdoption
	if err = d.ReadJSON("legacy-account-adoption.json", &retained); err != nil {
		return err
	}
	if notify.Hash(retained) != notify.Hash(a) {
		return fmt.Errorf("Signal migration adoption changed")
	}
	if _, err = relay.ReadLegacyInboxBootstrap(ctx, a.Spec.Inbox, a.Recovery.ResultHash); err != nil {
		return err
	}
	if !revoke {
		source, account, err := notification.LegacySignalSelectors(c.Policy.Notifications)
		if err != nil {
			return err
		}
		if source != s.Signal.Selectors {
			return fmt.Errorf("reviewed legacy Signal selectors changed")
		}
		if _, err = notification.LegacyCredentialHash(c.Policy.Notifications, s.signalProfile()); err != nil {
			return err
		}
		managed, ok := session.(install.ManagedSignalMigrationSession)
		if !ok {
			return fmt.Errorf("gateway lacks Signal migration support")
		}
		profile, err := managed.SignalMigrationProfile(ctx, a.Spec.accountState(), a.Recovery.ResultHash, account, c.Policy.Notifications.Primary, c.Policy.Notifications.Fallback)
		if err != nil {
			return err
		}
		if notify.Hash(profile) != notify.Hash(s.Signal.Profile) {
			return fmt.Errorf("Signal migration profile changed")
		}
	}
	return nil
}
