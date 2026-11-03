package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

type backupCommandRuntime struct {
	Version  int                `json:"version"`
	Producer notify.Producer    `json:"producer"`
	Identity notify.Identity    `json:"identity"`
	Socket   string             `json:"socket"`
	Policy   interaction.Config `json:"policy"`
}

func readBackupCommandRuntime(path string) (c backupCommandRuntime, err error) {
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	defer d.Close()
	if err = d.ReadJSON(filepath.Base(path), &c); err != nil {
		return c, err
	}
	if c.Version != 1 || !c.Identity.Valid() || !c.Producer.Valid() || !filepath.IsAbs(c.Socket) {
		return c, fmt.Errorf("invalid backup command runtime")
	}
	_, err = backupCommandHandlers(c.Policy, c.Producer, false, nil)
	return c, err
}

func runBackupCommandCLI(args []string) error {
	op := args[0]
	flags := flag.NewFlagSet("backup-worker "+op, flag.ContinueOnError)
	local := flags.Bool("local", false, "Explicit local staging")
	config := flags.String("config", "", "Private command-runtime JSON")
	stateDir := flags.String("state-dir", "", "Existing backup state directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || !*local {
		return fmt.Errorf("backup commands require --local and no positional arguments")
	}
	if op == "commands-manifest" {
		if *config != "" || *stateDir != "" {
			return fmt.Errorf("manifest accepts only --local")
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Version  int                               `json:"version"`
			Commands map[string]interaction.Descriptor `json:"commands"`
		}{1, backupCommandManifest(false)})
	}
	if !filepath.IsAbs(*config) || !filepath.IsAbs(*stateDir) {
		return fmt.Errorf("command config and backup state paths must be absolute")
	}
	c, err := readBackupCommandRuntime(*config)
	if err != nil {
		return err
	}
	if ui.DryRun {
		fmt.Printf("[DRY] %s: validated staged read-only backup command configuration\n", op)
		return nil
	}
	root, err := safefs.Open(*stateDir)
	if err != nil {
		return err
	}
	defer root.Close()
	lease, err := root.Lock("commands.lock", true, true)
	if err != nil {
		return err
	}
	defer lease.Close()
	create := op == "commands-init"
	if create {
		var gate installationGate
		if e := root.ReadJSON("installation.pending", &gate); e == nil {
			return fmt.Errorf("worker installation recovery is pending")
		} else if !os.IsNotExist(e) {
			return e
		}
		schema, err := root.Lock("schema.lock", true, true)
		if err != nil {
			return err
		}
		defer schema.Close()
	}
	state, err := openBackupStateWithLease(*stateDir, false, false, create)
	if err != nil {
		return err
	}
	defer state.Close()
	var control ArchiveControl
	if err = state.get("control", &control); err != nil {
		return err
	}
	if control.WriterID != c.Producer.ID || control.ArchiveID == "" || control.Epoch == 0 {
		return fmt.Errorf("backup command producer does not match local archive authority")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	initialize := func(tx *sql.Tx) error {
		if err := initializeBackupCommands(tx); err != nil {
			return err
		}
		if _, err := tx.Exec("CREATE TABLE backup_command_binding(singleton INTEGER PRIMARY KEY CHECK(singleton=1),archive_id TEXT NOT NULL,epoch INTEGER NOT NULL)"); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO backup_command_binding VALUES(1,?,?)", control.ArchiveID, control.Epoch)
		return err
	}
	ledger, err := interaction.OpenDomainLedger(ctx, state.db, c.Producer, c.Identity, create, initialize)
	if err != nil {
		return err
	}
	var archive string
	var epoch uint64
	if err = state.db.QueryRow("SELECT archive_id,epoch FROM backup_command_binding WHERE singleton=1").Scan(&archive, &epoch); err != nil {
		return err
	}
	if archive != control.ArchiveID || epoch != control.Epoch {
		return fmt.Errorf("backup authority changed; command reconciliation required")
	}
	if create {
		fmt.Println("Staged backup command ledger initialized; no service enabled.")
		return nil
	}
	handlers, err := backupCommandHandlers(c.Policy, c.Producer, false, nil)
	if err != nil {
		return err
	}
	return (client.Runner{Client: client.Client{Socket: c.Socket, Identity: c.Identity}, Producer: c.Producer, Ledger: ledger, Handlers: handlers}).Step(ctx)
}
