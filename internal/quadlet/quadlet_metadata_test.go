package quadlet

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"text/template"

	"github.com/dryaf/deploy/internal/config"
)

func TestQuadletMetadataRoundTripAndRoles(t *testing.T) {
	for _, mode := range []config.DeploymentMode{config.ModeBinary, config.ModeImage} {
		cfg := imageConfig()
		cfg.Deployment.Mode = mode
		if mode == config.ModeBinary {
			cfg.BinaryName = "app"
		}
		cfg.AppName = `quoted "app" <name>`
		n, err := config.Normalize(cfg, "prod")
		if err != nil {
			t.Fatal(err)
		}
		unit, err := Render(n, "")
		if err != nil {
			t.Fatal(err)
		}
		marker, err := ParseMetadata(strings.NewReader(unit))
		if err != nil || marker == nil || marker.App != cfg.AppName || marker.Mode != mode || marker.Backup.Enabled {
			t.Fatal(marker, err)
		}
		if !strings.Contains(unit, "io.deploy.service=pdf") {
			t.Fatal("runtime identity missing")
		}
		if strings.Count(unit, MetadataPrefix) != 1 {
			t.Fatal("multiple markers")
		}
	}
	for _, tc := range []struct {
		tmpl string
		data any
		role string
	}{{TraefikContainerTmpl, TraefikTemplateData{}, "infrastructure"}, {MaintenanceContainerTmpl, MaintenanceTemplateData{ServiceName: "foodlog"}, "maintenance"}} {
		parsed, err := template.New("unit").Parse(tc.tmpl)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err = parsed.Execute(&b, tc.data); err != nil {
			t.Fatal(err)
		}
		m, err := ParseMetadata(&b)
		if err != nil || m.Role != tc.role || m.Backup != nil {
			t.Fatal(m, err)
		}
	}
}

func TestQuadletMetadataRejectsAmbiguity(t *testing.T) {
	valid := `{"schema":1,"managed_by":"deploy","role":"infrastructure","service":"traefik"}`
	for name, text := range map[string]string{
		"duplicate key":    strings.Replace(valid, `"schema":1`, `"schema":1,"schema":1`, 1),
		"nested duplicate": strings.Replace(valid, `"schema":1`, `"schema":1,"extension":{"x":1,"x":2}`, 1),
		"wrong type":       strings.Replace(valid, `"schema":1`, `"schema":"1"`, 1),
		"trailing":         valid + ` {}`, "newline": strings.Replace(valid, "traefik", `traefik\nExecStart=evil`, 1),
		"nonapplication backup": strings.Replace(valid, `"schema":1`, `"schema":1,"backup":{"enabled":false}`, 1),
		"oversized":             strings.Repeat(" ", maxMetadataBytes) + valid,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseMetadata(strings.NewReader(MetadataPrefix + text + "\n[Unit]\n")); err == nil {
				t.Fatal("invalid marker accepted")
			}
		})
	}
	if _, err := ParseMetadata(strings.NewReader(MetadataPrefix + valid + "\n" + MetadataPrefix + valid)); err == nil {
		t.Fatal("duplicate markers accepted")
	}
	m, err := ParseMetadata(strings.NewReader(MetadataPrefix + strings.Replace(valid, `"schema":1`, `"schema":2`, 1)))
	if m == nil || m.Schema != 2 || !errors.Is(err, ErrUnsupportedMetadata) {
		t.Fatal("unsupported schema not retained", m, err)
	}
	if _, err := ParseMetadata(strings.NewReader(strings.Repeat("x", MaxQuadletBytes+1))); err == nil {
		t.Fatal("oversized file accepted")
	}
	if _, err := ParseMetadata(strings.NewReader(MetadataPrefix + strings.Replace(valid, `"schema":1`, `"schema":1,"description":"future additive field"`, 1))); err != nil {
		t.Fatal(err)
	}
}
