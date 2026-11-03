package quadlet

import (
	"bytes"
	"fmt"
	"path"
	"strconv"
	"strings"
	"text/template"

	"github.com/dryaf/deploy/internal/config"
)

const ImagePlaceholder = "__DEPLOY_RESOLVED_IMAGE__"

func ReleaseLabels(unit, commit, id string) string {
	return strings.Replace(unit, "[Container]\n", "[Container]\nLabel="+strconv.Quote("io.deploy.commit="+commit)+"\nLabel="+strconv.Quote("io.deploy.release="+id)+"\n", 1)
}

// renderQuadlet has no filesystem or process side effects.
func Render(n config.NormalizedConfig, resolvedImage string) (string, error) {
	q := n.Env.Quadlet
	if resolvedImage != "" {
		q.Image = resolvedImage
	}
	if q.Description == "" {
		q.Description = q.ServiceName + " Service"
	}
	labels, err := config.MergeLabels(q.Labels, config.TraefikLabels(q.ServiceName, q.Router, "myresolver"))
	if err != nil {
		return "", err
	}
	q.Labels = labels
	q.Labels = append(q.Labels, "io.deploy.managed-by=deploy", "io.deploy.role=application", "io.deploy.service="+q.ServiceName)
	q.Volumes = append([]string(nil), q.Volumes...)
	for i, v := range q.Volumes {
		parts := strings.Split(v, ":")
		if strings.HasPrefix(parts[0], "./") {
			parts[0] = path.Join(n.Env.Dir, parts[0])
			q.Volumes[i] = strings.Join(parts, ":")
		}
	}
	data := TemplateData{Quadlet: q, TargetDir: n.Env.Dir, Routing: n.Routing, EnvFile: n.EnvFile, CPUQuota: n.CPUQuota, Pinned: n.Mode == config.ModeImage}
	// systemd expands dollar variables in ExecStartPre even inside quoted argv.
	data.ActivationGate = strings.ReplaceAll(path.Join(n.Env.Dir, ".deploy", string(n.Mode), "activation-check.sh"), "$", "$$")
	t, err := template.New("quadlet").Funcs(template.FuncMap{
		"literal": func(s string) string { return strings.ReplaceAll(s, "%", "%%") },
		"quote":   func(s string) string { return strconv.Quote(strings.ReplaceAll(s, "%", "%%")) },
	}).Parse(quadletTemplate)
	if err != nil {
		return "", fmt.Errorf("parse Quadlet: %w", err)
	}
	var b bytes.Buffer
	marker, err := EncodeMetadata(applicationMetadata(n))
	if err != nil {
		return "", err
	}
	b.WriteString(marker)
	if err = t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("render Quadlet: %w", err)
	}
	return b.String(), nil
}
