package inventory

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/transport"
)

//go:embed inventory_probe.py
var inventoryProbe string

type Inventory struct {
	Schema      int              `json:"schema"`
	UID         int              `json:"uid"`
	Complete    bool             `json:"complete"`
	ObservedAt  time.Time        `json:"observed_at"`
	Entries     []InventoryEntry `json:"entries"`
	Diagnostics []string         `json:"diagnostics"`
}

type InventoryEntry struct {
	Dropins       bool                     `json:"dropins"`
	ID            string                   `json:"id"`
	Service       string                   `json:"service,omitempty"`
	SourcePath    string                   `json:"source_path,omitempty"`
	ContainerID   string                   `json:"container_id,omitempty"`
	ContainerName string                   `json:"container_name,omitempty"`
	Management    string                   `json:"management"`
	Ownership     string                   `json:"ownership"`
	State         string                   `json:"state"`
	Backup        string                   `json:"backup"`
	Metadata      *quadlet.QuadletMetadata `json:"metadata,omitempty"`
	Diagnostics   []string                 `json:"diagnostics"`
}

type InventoryProbeResult struct {
	Schema      int                          `json:"schema"`
	UID         int                          `json:"uid"`
	Complete    bool                         `json:"complete"`
	Files       []InventoryFile              `json:"files"`
	Units       map[string]map[string]string `json:"units"`
	Containers  []inventoryContainer         `json:"containers"`
	Diagnostics []string                     `json:"diagnostics"`
}

type InventoryFile struct {
	Path         string             `json:"path"`
	ResolvedPath string             `json:"resolved_path"`
	Name         string             `json:"name"`
	Precedence   int                `json:"precedence"`
	Masked       bool               `json:"masked"`
	Content      string             `json:"content"`
	Dropins      bool               `json:"dropins"`
	Error        string             `json:"error"`
	Evidence     *InventoryEvidence `json:"evidence"`
}

type InventoryEvidence struct {
	Owner           InventoryOwner `json:"owner"`
	TargetOwner     InventoryOwner `json:"target_owner"`
	Current         InventoryOwner `json:"current"`
	CandidateSHA256 string         `json:"candidate_sha256"`
	Pending         bool           `json:"pending"`
}

type InventoryOwner struct {
	Schema  int                   `json:"schema"`
	Service string                `json:"service"`
	Target  string                `json:"target"`
	Mode    config.DeploymentMode `json:"mode"`
	Current string                `json:"current"`
}

type inventoryContainer struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	State  string            `json:"state"`
	Labels map[string]string `json:"labels"`
}

func Collect(remote *config.Environment) (Inventory, error) {
	cmd := exec.Command("python3", "-I", "-c", inventoryProbe)
	if remote != nil {
		// No remote installation, generation, reload, service action, or enrollment.
		args := append([]string{"-o", "ControlMaster=no", "-o", "ControlPath=none"}, transport.GetSSHBaseArgs(*remote)...)
		args = append(args, "python3 -I -c "+transport.ShellQuote(inventoryProbe))
		cmd = exec.Command("ssh", args...)
	}
	var out, stderr process.Buffer
	out.Limit = 16 << 20
	stderr.Limit = 4096
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := process.Run(cmd, 30*time.Second, 2*time.Second); err != nil {
		return Inventory{}, fmt.Errorf("inventory probe failed (requires Python 3, Podman and systemd): %w", err)
	}
	var raw InventoryProbeResult
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil || raw.Schema != 1 {
		return Inventory{}, fmt.Errorf("invalid inventory probe response")
	}
	return Merge(raw), nil
}

func Merge(raw InventoryProbeResult) Inventory {
	out := Inventory{Schema: 1, UID: raw.UID, Complete: raw.Complete, ObservedAt: time.Now().UTC(), Entries: []InventoryEntry{}, Diagnostics: append([]string{}, raw.Diagnostics...)}
	files := append([]InventoryFile(nil), raw.Files...)
	sort.Slice(files, func(i, j int) bool {
		if files[i].Precedence != files[j].Precedence {
			return files[i].Precedence < files[j].Precedence
		}
		return files[i].Path < files[j].Path
	})
	active := map[string]string{}
	discoveredPaths := map[string]bool{}
	for _, file := range files {
		discoveredPaths[filepath.Clean(file.Path)] = true
	}
	matchedContainers := map[string]bool{}
	seenUnits := map[string]bool{}
	containers := map[string]inventoryContainer{}
	for _, c := range raw.Containers {
		containers[c.ID] = c
	}
	for _, f := range files {
		service := strings.TrimSuffix(f.Name, ".container")
		row := InventoryEntry{ID: inventoryID("quadlet", f.Path), Service: service, SourcePath: f.Path, Management: "unmarked", Ownership: "unproven", State: "configured-but-not-created", Backup: "—", Diagnostics: []string{}}
		unit := raw.Units[service+".service"]
		seenUnits[service+".service"] = true
		source := unit["SourcePath"]
		shadowed := false
		if source != "" {
			shadowed = filepath.Clean(source) != filepath.Clean(f.Path)
			if !discoveredPaths[filepath.Clean(source)] && active[f.Name] == "" && f.ResolvedPath != "" && f.ResolvedPath == unit["ResolvedSourcePath"] {
				shadowed = false
			}
		} else if previous := active[f.Name]; previous != "" {
			shadowed = previous != f.Path
		}
		if !shadowed {
			active[f.Name] = f.Path
		}
		if f.Error != "" {
			row.State = "inaccessible"
			row.Diagnostics = append(row.Diagnostics, f.Error)
			out.Complete = false
		}
		if f.Masked {
			row.State = "masked"
		} else if f.Error == "" {
			m, err := quadlet.ParseMetadata(strings.NewReader(f.Content))
			row.Metadata = m
			if err != nil {
				row.Management = "malformed-marker"
				if errors.Is(err, quadlet.ErrUnsupportedMetadata) {
					row.Management = "unsupported-schema"
				}
				row.Diagnostics = append(row.Diagnostics, err.Error())
			} else if m != nil {
				row.Management = "deploy"
				if m.Service != service {
					row.Management = "metadata-drift"
					row.Diagnostics = append(row.Diagnostics, "metadata service does not match Quadlet filename")
				}
				if m.Role == "application" && m.Backup != nil {
					row.Backup = "disabled"
					if m.Backup.Enabled {
						row.Backup = "enabled"
					}
				}
			}
			if validInventoryEvidence(f, service) {
				row.Ownership = "confirmed"
				if f.Evidence.Pending {
					row.Ownership = "pending"
					row.Diagnostics = append(row.Diagnostics, "unfinished deployment/database/boot transaction")
				}
				if m == nil && err == nil {
					row.Management = "legacy-managed"
				}
				if m != nil && (m.TargetDir != f.Evidence.Owner.Target || m.Mode != f.Evidence.Owner.Mode) {
					row.Ownership = "drift"
					row.Diagnostics = append(row.Diagnostics, "metadata differs from committed ownership")
				}
			} else if m != nil && m.Role == "application" {
				row.Diagnostics = append(row.Diagnostics, "matching committed service/target ownership not established")
			}
			if f.Evidence != nil && f.Evidence.Pending && row.Ownership != "pending" {
				row.Ownership = "pending"
				row.Diagnostics = append(row.Diagnostics, "unfinished transaction; installed candidate is not confirmed")
			}
		}
		if shadowed {
			row.State = "shadowed"
			row.Diagnostics = append(row.Diagnostics, "another Quadlet source takes precedence")
		} else if !f.Masked && f.Error == "" && unit != nil {
			switch unit["LoadState"] {
			case "masked":
				row.State = "masked"
			case "not-found":
			case "error", "bad-setting":
				row.State = "failed"
			default:
				if unit["ActiveState"] != "" {
					row.State = unit["ActiveState"]
				}
			}
			if source == "" {
				row.Diagnostics = append(row.Diagnostics, "effective unit has no proven Quadlet SourcePath")
			}
			if cid := unit["ContainerID"]; cid != "" && source != "" {
				if c, ok := containers[cid]; ok && !matchedContainers[cid] {
					row.ContainerID, row.ContainerName = c.ID, c.Name
					row.State = c.State
					matchedContainers[cid] = true
					if c.Labels["io.deploy.service"] != "" && c.Labels["io.deploy.service"] != service {
						row.Diagnostics = append(row.Diagnostics, "runtime service label differs from effective unit")
					}
				}
			}
		}
		row.Dropins = f.Dropins || unit["DropInPaths"] != ""
		if row.Dropins {
			row.Diagnostics = append(row.Diagnostics, "drop-ins require effective database binding verification")
		}
		if row.Backup == "enabled" {
			row.Diagnostics = append(row.Diagnostics, "opt-in only; exporter must revalidate ownership and effective mounts under locks")
		}
		out.Entries = append(out.Entries, row)
	}
	for name, u := range raw.Units {
		if seenUnits[name] || !strings.HasSuffix(u["SourcePath"], ".container") {
			continue
		}
		out.Entries = append(out.Entries, InventoryEntry{ID: inventoryID("unit", name), Service: strings.TrimSuffix(name, ".service"), SourcePath: u["SourcePath"], Management: "unmarked", Ownership: "unproven", State: u["ActiveState"], Backup: "—", Diagnostics: []string{"generated unit has no discovered Quadlet source"}})
	}
	for _, c := range raw.Containers {
		if matchedContainers[c.ID] {
			continue
		}
		row := InventoryEntry{ID: inventoryID("container", c.ID), ContainerID: c.ID, ContainerName: c.Name, Management: "unmarked", Ownership: "unproven", State: c.State, Backup: "—", Diagnostics: []string{"container not associated with an effective unit; names and labels alone do not prove association"}}
		if c.Labels["io.deploy.managed-by"] == "deploy" {
			row.Management = "orphaned-claim"
		}
		out.Entries = append(out.Entries, row)
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.ID < b.ID
	})
	return out
}

func inventoryID(kind, value string) string {
	return fmt.Sprintf("%s-%x", kind, sha256.Sum256([]byte(value)))
}

func validInventoryEvidence(f InventoryFile, service string) bool {
	e := f.Evidence
	if e == nil || e.Current.Current == "" {
		return false
	}
	mode := e.Owner.Mode
	if mode != "binary" && mode != "image" {
		return false
	}
	for _, o := range []InventoryOwner{e.Owner, e.TargetOwner, e.Current} {
		if o.Schema != 1 || o.Service != service || o.Target != e.Owner.Target || !filepath.IsAbs(o.Target) || o.Target == "/" {
			return false
		}
	}
	if e.TargetOwner.Mode != mode {
		return false
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(f.Content))) == e.CandidateSHA256
}

func Print(w io.Writer, inv Inventory, managed, jsonOutput bool) error {
	if managed {
		filtered := make([]InventoryEntry, 0)
		for _, r := range inv.Entries {
			if r.Management != "unmarked" {
				filtered = append(filtered, r)
			} else {
				for _, d := range r.Diagnostics {
					inv.Diagnostics = append(inv.Diagnostics, r.ID+": "+d)
				}
			}
		}
		inv.Entries = filtered
	}
	if jsonOutput {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inv)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE / CONTAINER\tMANAGEMENT\tAPP\tENV\tSTATE\tBACKUP")
	for _, r := range inv.Entries {
		name := r.Service
		if name == "" {
			name = r.ContainerName
		}
		app, env := "—", "—"
		if r.Metadata != nil {
			if r.Metadata.App != "" {
				app = r.Metadata.App
			}
			if r.Metadata.Environment != "" {
				env = r.Metadata.Environment
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", DisplayInventoryText(name), r.Management, DisplayInventoryText(app), DisplayInventoryText(env), DisplayInventoryText(r.State), r.Backup)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, r := range inv.Entries {
		for _, d := range r.Diagnostics {
			fmt.Fprintf(w, "  %s: %s\n", DisplayInventoryText(r.SourcePath+r.ContainerName), DisplayInventoryText(d))
		}
	}
	for _, d := range inv.Diagnostics {
		fmt.Fprintf(w, "  inventory: %s\n", DisplayInventoryText(d))
	}
	if !inv.Complete {
		fmt.Fprintln(w, "Inventory incomplete; unavailable data is not an empty inventory.")
	}
	return nil
}

func DisplayInventoryText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || (r >= 0x80 && r <= 0x9f) {
			return '�'
		}
		return r
	}, s)
}
