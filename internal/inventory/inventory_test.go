package inventory

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/testutil"
)

func inventoryFixture(t *testing.T) InventoryProbeResult {
	t.Helper()
	cfg := imageConfig()
	n, err := config.Normalize(cfg, "prod")
	if err != nil {
		t.Fatal(err)
	}
	unit, err := quadlet.Render(n, "")
	if err != nil {
		t.Fatal(err)
	}
	owner := InventoryOwner{Schema: 1, Service: "pdf", Target: "/srv/pdf", Mode: config.ModeImage, Current: "release-1"}
	cid := strings.Repeat("a", 64)
	return InventoryProbeResult{Schema: 1, UID: 1000, Complete: true, Files: []InventoryFile{{Path: "/home/user/.config/containers/systemd/pdf.container", Name: "pdf.container", Content: unit, Evidence: &InventoryEvidence{Owner: owner, TargetOwner: owner, Current: owner, CandidateSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(unit)))}}}, Units: map[string]map[string]string{"pdf.service": {"SourcePath": "/home/user/.config/containers/systemd/pdf.container", "ActiveState": "active", "LoadState": "loaded", "ContainerID": cid}}, Containers: []inventoryContainer{{ID: cid, Name: "custom-container", State: "running", Labels: map[string]string{"io.deploy.service": "pdf"}}}}
}

func TestInventoryMergeProvesAssociation(t *testing.T) {
	raw := inventoryFixture(t)
	inv := Merge(raw)
	if !inv.Complete || len(inv.Entries) != 1 {
		t.Fatal(inv)
	}
	r := inv.Entries[0]
	if r.Management != "deploy" || r.Ownership != "confirmed" || r.ContainerName != "custom-container" || r.State != "running" {
		t.Fatal(r)
	}
	raw.Units["pdf.service"]["ContainerID"] = ""
	raw.Containers[0].Name = "systemd-pdf"
	inv = Merge(raw)
	if len(inv.Entries) != 2 {
		t.Fatal("name alone incorrectly associated container", inv)
	}
	if inv.Entries[1].ContainerID != "" {
		t.Fatal("unit got unproven container")
	}
}

func TestInventoryDriftPendingAndLegacy(t *testing.T) {
	for name, edit := range map[string]func(*InventoryProbeResult){
		"owner missing":   func(r *InventoryProbeResult) { r.Files[0].Evidence = nil },
		"target conflict": func(r *InventoryProbeResult) { r.Files[0].Evidence.TargetOwner.Target = "/srv/other" },
		"candidate drift": func(r *InventoryProbeResult) { r.Files[0].Content += "\n# changed\n" },
	} {
		t.Run(name, func(t *testing.T) {
			raw := inventoryFixture(t)
			edit(&raw)
			if Merge(raw).Entries[0].Ownership == "confirmed" {
				t.Fatal("unproven ownership confirmed")
			}
		})
	}
	raw := inventoryFixture(t)
	raw.Files[0].Evidence.Pending = true
	if r := Merge(raw).Entries[0]; r.Ownership != "pending" {
		t.Fatal(r)
	}
	raw = inventoryFixture(t)
	raw.Files[0].Content = "[Container]\nImage=legacy\n"
	raw.Files[0].Evidence.CandidateSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(raw.Files[0].Content)))
	if r := Merge(raw).Entries[0]; r.Management != "legacy-managed" {
		t.Fatal(r)
	}
	raw.Files[0].Evidence = nil
	if r := Merge(raw).Entries[0]; r.Management != "unmarked" {
		t.Fatal(r)
	}
}

func TestInventoryShadowMasksMalformedAndPartial(t *testing.T) {
	raw := inventoryFixture(t)
	other := raw.Files[0]
	other.Path = "/etc/containers/systemd/users/pdf.container"
	other.Precedence = 2
	raw.Files = append(raw.Files, other)
	inv := Merge(raw)
	states := map[string]int{}
	for _, r := range inv.Entries {
		states[r.State]++
	}
	if states["shadowed"] != 1 || states["running"] != 1 {
		t.Fatal(inv)
	}
	raw = inventoryFixture(t)
	raw.Files[0].Masked = true
	raw.Files[0].Content = ""
	raw.Units["pdf.service"]["LoadState"] = "masked"
	if r := Merge(raw).Entries[1]; r.State != "masked" {
		t.Fatal(r)
	}
	raw = inventoryFixture(t)
	raw.Files[0].Content = quadlet.MetadataPrefix + "{bad}"
	if r := Merge(raw).Entries[0]; r.Management != "malformed-marker" {
		t.Fatal(r)
	}
	raw = InventoryProbeResult{Schema: 1, Complete: false, Diagnostics: []string{"Podman inventory unavailable"}}
	inv = Merge(raw)
	var b bytes.Buffer
	if err := Print(&b, inv, true, true); err != nil {
		t.Fatal(err)
	}
	var decoded Inventory
	if err := json.Unmarshal(b.Bytes(), &decoded); err != nil || decoded.Complete || len(decoded.Diagnostics) == 0 {
		t.Fatal(decoded, err)
	}
}

func TestInventoryManagedFilterKeepsDiagnostics(t *testing.T) {
	inv := Inventory{Schema: 1, Complete: false, Entries: []InventoryEntry{{ID: "unmarked", Management: "unmarked", Diagnostics: []string{"association ambiguous"}}}, Diagnostics: []string{"systemd failed"}}
	var b bytes.Buffer
	if err := Print(&b, inv, true, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "association ambiguous") || !strings.Contains(b.String(), "systemd failed") {
		t.Fatal(b.String())
	}
}

func TestInventoryProbeReadOnlyAndBounded(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("Python 3 unavailable")
	}
	root := t.TempDir()
	bin := t.TempDir()
	testutil.WriteFixture(t, root, "example.container", "[Container]\nImage=example\n")
	if err := os.Symlink("/dev/null", filepath.Join(root, "masked.container")); err != nil {
		t.Fatal(err)
	}
	// Only expected read commands are accepted by these fake host tools.
	systemctl := `#!/bin/sh
case "$*" in
 *list-unit-files*) exit 0;;
 *show*) printf '%s\n' 'Id=example.service' 'LoadState=not-found' 'ActiveState=inactive';;
 *) exit 99;;
esac
`
	podman := `#!/bin/sh
case "$1 $2" in 'ps --all') exit 0;; *) exit 99;; esac
`
	for name, body := range map[string]string{"systemctl": systemctl, "podman": podman} {
		p := testutil.WriteFixture(t, bin, name, body)
		if err := os.Chmod(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	probe := strings.Replace(inventoryProbe, "seen_paths = set()", "roots = [sys.argv[1]]\nseen_paths = set()", 1)
	cmd := exec.Command("python3", "-I", "-c", probe, root)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	output, err := process.Output(cmd, 5*time.Second)
	if err != nil {
		t.Fatal(err, string(output))
	}
	var raw InventoryProbeResult
	if err = json.Unmarshal(output, &raw); err != nil {
		t.Fatal(err, string(output))
	}
	if len(raw.Files) != 2 || !raw.Files[1].Masked {
		t.Fatal(raw)
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 2 {
		t.Fatal("probe wrote files", err)
	}
	// Partial provider failure must survive the entire executable path.
	testutil.WriteFixture(t, bin, "podman", "#!/bin/sh\nexit 1\n")
	cmd = exec.Command("python3", "-I", "-c", probe, root)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	output, err = process.Output(cmd, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(output, &raw); err != nil || raw.Complete {
		t.Fatal("failure turned into empty healthy inventory", err)
	}
}

func TestInventorySymlinkAliasDoesNotActivateShadow(t *testing.T) {
	raw := inventoryFixture(t)
	raw.Files[0].ResolvedPath = "/srv/units/pdf.container"
	raw.Units["pdf.service"]["ResolvedSourcePath"] = raw.Files[0].ResolvedPath
	shadow := raw.Files[0]
	shadow.Path = "/etc/containers/systemd/users/pdf.container"
	shadow.Precedence = 3
	raw.Files = append(raw.Files, shadow)
	inv := Merge(raw)
	count := 0
	for _, row := range inv.Entries {
		if row.State == "shadowed" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("a lower-priority symlink appeared active", inv)
	}
}
