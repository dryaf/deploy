package interaction

import (
	"context"
	"errors"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

type installationGatewayFixture struct {
	commitGatewayFixture
	calls  int
	change func(*CommandRecoveryInstallation) error
}

func (s *installationGatewayFixture) InstallCommandRecoveryOutcomes(_ context.Context, proof CommandRecoveryCommit) (v CommandRecoveryInstallation, err error) {
	s.calls++
	v = CommandRecoveryInstallation{Version: 1, Checkpoint: proof, OutcomesHash: protocol.Hash("projection"), Commands: proof.Summary.Manifest.Commands}
	if s.change != nil {
		err = s.change(&v)
	}
	return
}

func TestCommandInstallationRequiresCommittedSource(t *testing.T) {
	l, _, _, gateway := commandCommitFixture(t)
	install := &installationGatewayFixture{commitGatewayFixture: gateway}
	if _, err := InstallCommandRecovery(context.Background(), l, install); !errors.Is(err, protocol.ErrRecoveryIncomplete) {
		t.Fatal(err)
	}
	if install.calls != 0 {
		t.Fatal("gateway called before source commit")
	}
	_, err := CommitCommandRecovery(context.Background(), l, gateway)
	testutil.Must(t, err)
	_, err = InstallCommandRecovery(context.Background(), l, install)
	testutil.Must(t, err)
	if install.calls != 1 {
		t.Fatal("installation missing")
	}
}

func TestCommandInstallationRejectsChangedResponseOrSource(t *testing.T) {
	for _, mode := range []string{"version", "checkpoint", "enabled", "count", "digest", "source", "cancel", "lost_response"} {
		t.Run(mode, func(t *testing.T) {
			l, db, _, gateway := commandCommitFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := CommitCommandRecovery(ctx, l, gateway)
			testutil.Must(t, err)
			install := &installationGatewayFixture{commitGatewayFixture: gateway}
			install.change = func(v *CommandRecoveryInstallation) error {
				switch mode {
				case "version":
					v.Version++
				case "checkpoint":
					v.Checkpoint.ResultFloorsHash = protocol.Hash("another")
				case "enabled":
					v.ExecutionEnabled = true
				case "count":
					v.Commands++
				case "digest":
					v.OutcomesHash = ""
				case "source":
					_, err = db.Exec("UPDATE interactive_inbox SET fenced=0")
					testutil.Must(t, err)
				case "cancel":
					cancel()
				case "lost_response":
					return errors.New("lost installation response")
				}
				return nil
			}
			if _, err = InstallCommandRecovery(ctx, l, install); err == nil {
				t.Fatal("changed installation accepted")
			}
			if install.calls != 1 {
				t.Fatal("gateway callback not tested")
			}
			if mode == "lost_response" {
				install.change = nil
				_, err = InstallCommandRecovery(ctx, l, install)
				testutil.Must(t, err)
			}
		})
	}
}
