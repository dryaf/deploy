package install

import (
	"fmt"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/safefs"
)

func readRelayConfiguration(path string) (c relay.Config, err error) {
	dir, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	defer dir.Close()
	if err = dir.ReadJSON(filepath.Base(path), &c); err != nil {
		return c, err
	}
	for _, p := range []string{c.StateDir, c.Socket, c.AccountFile, c.RecipientFile} {
		if !filepath.IsAbs(p) {
			return c, fmt.Errorf("relay paths must be absolute")
		}
	}
	if c.Interactive != nil {
		if err = c.Interactive.Validate(); err != nil {
			return c, err
		}
		if filepath.Clean(c.Interactive.InboxDir) == filepath.Clean(c.StateDir) {
			return c, fmt.Errorf("relay inbox and Signal account directories must be separate")
		}
	}
	return
}
