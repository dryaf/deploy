package workerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/dryaf/deploy/internal/config"
)

func (c CLI) configure(ctx context.Context, kind string, v Invocation, inv config.ServerInventory) error {
	e, err := OpenInventoryEdit(v.Options.Inventory)
	if err != nil {
		return err
	}
	d := inv.Servers[v.Server].Desired
	missing := MissingForWorker(kind, *d)
	if len(missing) == 0 {
		if v.Options.JSON {
			return PrintPlan(c.Out, Plan{Version: 1, Server: v.Server, Worker: kind, Operation: "configure", State: "unchanged", Code: "ok", Next: "apply to inspect target prerequisites and credential files"}, true)
		}
		fmt.Fprintln(c.Out, "All declaration inputs are present; apply to inspect the target and credential files.")
		return nil
	}
	if v.Options.NonInteractive {
		return fmt.Errorf("missing_input: %s", strings.Join(missing, ", "))
	}
	prompt := c.Prompt
	if prompt == nil {
		prompt = TerminalPrompt{Input: os.Stdin, Output: c.Err}
	}
	// Re-evaluate after provider selection because it reveals credential inputs.
	for round := 0; round < 4; round++ {
		if len(missing) == 0 {
			break
		}
		for _, field := range missing {
			label := field + " (target file path, local:PATH, or new for hidden input)"
			if field == "notification_worker.providers.telegram.bot_name" {
				label = "Telegram bot username (without @)"
			}
			if field == "notification_worker.providers" {
				label = "Notification providers (telegram, signal, or telegram,signal)"
			}
			if field == "backup_worker.sources" {
				label = "Backup source server aliases (comma separated)"
			}
			if strings.HasSuffix(field, "routes.alerts.primary") {
				label = "Primary notification provider (telegram or signal)"
			}
			answer, askErr := prompt.Ask(ctx, label, false)
			if askErr != nil {
				return askErr
			}
			if answer == "" {
				return fmt.Errorf("%s: a value is required", field)
			}
			keys := strings.Split(field, ".")
			var value any = answer
			switch field {
			case "notification_worker.providers", "backup_worker.sources":
				values := strings.Split(answer, ",")
				for i := range values {
					values[i] = strings.TrimSpace(values[i])
				}
				value = values
			case "notification_worker.routes.alerts.primary":
				if _, ok := d.Notification.Providers[answer]; !ok {
					return fmt.Errorf("choose a configured provider")
				}
				if err = e.Set(v.Server, keys, answer); err != nil {
					return err
				}
				for provider := range d.Notification.Providers {
					if provider != answer {
						if err = e.Set(v.Server, []string{"notification_worker", "routes", "alerts", "fallback"}, provider); err != nil {
							return err
						}
					}
				}
				continue
			default:
				if answer == "new" {
					if e.Mode&0200 == 0 {
						return fmt.Errorf("save a writable inventory before staging new credentials")
					}
					secret, secretErr := prompt.Ask(ctx, "Private "+field, true)
					if secretErr != nil {
						return secretErr
					}
					staged, stageErr := StageCredentialInput(e.File, v.Server, keys[len(keys)-2], keys[len(keys)-1], []byte(secret))
					if stageErr != nil {
						return stageErr
					}
					answer = "local:" + staged
				}
				if strings.HasPrefix(answer, "local:") {
					keys[len(keys)-1] = strings.TrimSuffix(keys[len(keys)-1], "_file") + "_input"
					value = strings.TrimPrefix(answer, "local:")
					if value == "" {
						return fmt.Errorf("local input requires a file path")
					}
				}
			}
			if err = e.Set(v.Server, keys, value); err != nil {
				return err
			}
		}
		candidate, candidateErr := e.Candidate()
		if candidateErr != nil {
			return candidateErr
		}
		next, parseErr := config.ParseInventory(e.File, candidate)
		if parseErr != nil {
			return parseErr
		}
		d = next.Servers[v.Server].Desired
		missing = MissingForWorker(kind, *d)
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing_input: %s", strings.Join(missing, ", "))
	}
	candidate, err := e.Candidate()
	if err != nil {
		return err
	}
	if e.Mode&0200 == 0 {
		// stdout is the candidate, allowing a deliberate redirect. No remote work or
		// hidden desired-policy persistence occurs for a read-only inventory.
		if v.Options.JSON {
			return json.NewEncoder(c.Out).Encode(struct {
				Version   int    `json:"version"`
				State     string `json:"state"`
				Code      string `json:"code"`
				Candidate string `json:"candidate"`
				Next      string `json:"next"`
			}{1, "candidate", "ok", string(candidate), "review and save the YAML candidate before applying"})
		}
		_, err = c.Out.Write(candidate)
		return err
	}
	next, err := config.ParseInventory(e.File, candidate)
	if err != nil {
		return err
	}
	changes := PolicyChanges(*inv.Servers[v.Server].Desired, *next.Servers[v.Server].Desired)
	if len(changes) > 0 {
		var summary strings.Builder
		fmt.Fprintf(&summary, "YAML changes for %s:\n", v.Server)
		for _, change := range changes {
			fmt.Fprintf(&summary, "  %s\n", change.Field)
		}
		// Show paths once without echoing private destinations or values. JSON
		// stdout remains one result; a failed display never silently saves.
		if _, err = c.Err.Write([]byte(summary.String())); err != nil {
			return fmt.Errorf("could not display configuration changes: %w", err)
		}
	}
	if err = e.Save(); err != nil {
		return err
	}
	if v.Options.JSON {
		return PrintPlan(c.Out, Plan{Version: 1, Server: v.Server, Worker: kind, Operation: "configure", State: "configured", Code: "ok", Next: "deploy " + kind + " " + v.Server}, true)
	}
	fmt.Fprintf(c.Out, "Saved %s. Apply with: deploy %s %s\n", v.Options.Inventory, kind, v.Server)
	return nil
}
