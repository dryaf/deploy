package install

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

func serveSignalService(ctx context.Context, root, operation string) error {
	if !safefs.ValidID(operation, "n") {
		return fmt.Errorf("Signal service operation pin required")
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
	m, err := readSignalService(d, "ready.json")
	if err != nil {
		return err
	}
	if m.Spec.Root != root || m.Spec.Operation != operation {
		return fmt.Errorf("Signal service runtime pins changed")
	}
	check := func() error {
		if err := checkSignalServiceFiles(d, m, true); err != nil {
			return err
		}
		if err := absentServiceFile(d, "remove.json"); err != nil {
			return err
		}
		j, err := readSignalServiceJournal(d, m)
		if err != nil {
			return err
		}
		if signalServicePhase(j.Phase) < signalServicePhase("published") || signalServicePhase(j.Phase) > signalServicePhase("complete") {
			return fmt.Errorf("Signal service is not published")
		}
		if j.Phase == "complete" {
			if err = verifySignalServiceComplete(d, m); err != nil {
				return err
			}
		}
		running, err := os.Stat("/proc/self/exe")
		if err != nil {
			return err
		}
		f, err := d.Open("deploy", unix.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		pinned, err := f.Stat()
		f.Close()
		if err != nil {
			return err
		}
		if !os.SameFile(running, pinned) {
			return fmt.Errorf("Signal verifier is not its installed executable")
		}
		held, err := lease.File().Stat()
		if err != nil {
			return err
		}
		f, err = d.Open("installation.lock", unix.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		current, err := f.Stat()
		f.Close()
		if err != nil {
			return err
		}
		if !os.SameFile(held, current) {
			return fmt.Errorf("Signal runtime lease replaced")
		}
		return nil
	}
	if err = check(); err != nil {
		return err
	}
	v, err := openSignalEndpointVerifierService(ctx, m.Spec.Endpoint.Root, m.Spec.Endpoint.ManifestHash, m.binding())
	if err != nil {
		return err
	}
	defer v.close()
	return serveSignalVerifier(ctx, v, time.Second, check)
}
