package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
)

func serveFreshSignal(ctx context.Context, root, operation string) error {
	if !safefs.ValidID(operation, "n") {
		return fmt.Errorf("fresh Signal operation pin required")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return err
	}
	defer d.Close()
	lease, err := d.Lock("installation.lock", false, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	m, err := readFreshSignal(d, "ready.json")
	if err != nil {
		return err
	}
	if m.Spec.Root != root || m.Spec.Operation != operation {
		return fmt.Errorf("fresh Signal runtime pins changed")
	}
	check := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := freshSignalNotRemoving(d); err != nil {
			return err
		}
		if err := checkFreshSignalFiles(d, m, true); err != nil {
			return err
		}
		j, err := readFreshSignalJournal(d, m)
		if err != nil {
			return err
		}
		if freshSignalPhase(j.Phase) < freshSignalPhase("published") || freshSignalPhase(j.Phase) > freshSignalPhase("complete") {
			return fmt.Errorf("fresh Signal service is not published")
		}
		if j.Phase == "complete" {
			if err = verifyFreshSignalComplete(d, m); err != nil {
				return err
			}
		}
		running, err := os.Stat("/proc/self/exe")
		if err != nil {
			return err
		}
		actual, err := os.Stat(m.executable())
		if err != nil {
			return err
		}
		if !os.SameFile(running, actual) {
			return fmt.Errorf("fresh Signal runtime is not its installed executable")
		}
		held, err := lease.File().Stat()
		if err != nil {
			return err
		}
		current, err := os.Stat(filepath.Join(root, "installation.lock"))
		if err != nil {
			return err
		}
		if !os.SameFile(held, current) {
			return fmt.Errorf("fresh Signal runtime lease replaced")
		}
		return nil
	}
	if err = check(ctx); err != nil {
		return err
	}
	account, err := signalaccount.OpenNativeRuntime(m.Spec.AccountState, m.binding(), func() error { return check(ctx) })
	if err != nil {
		return err
	}
	defer account.Close()
	inbox, err := relay.OpenFreshSignalInbox(ctx, m.Spec.Inbox, check)
	if err != nil {
		return err
	}
	defer inbox.Close()
	helper, err := d.Open("deploy", os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer helper.Close()
	cli, err := d.Open("signal-cli", os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer cli.Close()
	config := relay.Config{StateDir: m.Spec.AccountState, Socket: m.Spec.socket(), AccountFile: filepath.Join(root, "account"), RecipientFile: filepath.Join(root, "recipient")}
	execution := relay.FreshExecution{Account: account, Inbox: inbox, Check: check, Recipient: func(ctx context.Context, hash, destination string) (string, error) {
		if err := check(ctx); err != nil {
			return "", err
		}
		return freshSignalRecipient(m, hash, destination)
	}, Health: func(ctx context.Context) (any, error) {
		if err := check(ctx); err != nil {
			return nil, err
		}
		return freshSignalHealth(m)
	}, Command: func(ctx context.Context, lease *os.File, output io.Writer, args ...string) error {
		if err := check(ctx); err != nil {
			return err
		}
		for _, pair := range []struct {
			file *os.File
			name string
		}{{helper, "deploy"}, {cli, "signal-cli"}} {
			held, err := pair.file.Stat()
			if err != nil {
				return err
			}
			current, err := os.Stat(filepath.Join(root, pair.name))
			if err != nil {
				return err
			}
			if !os.SameFile(held, current) {
				return fmt.Errorf("fresh Signal execution descriptor replaced")
			}
		}
		return relay.RunPinnedCommand(ctx, helper, cli, lease, relay.CommandRequest{Version: 1, CLIHash: m.Spec.CLISHA256, State: config.StateDir, Args: args}, output)
	}}
	return relay.RunFreshManaged(ctx, config, execution)
}
