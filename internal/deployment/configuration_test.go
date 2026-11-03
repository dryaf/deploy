package deployment

import (
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/quadlet"
)

func TestMemoryLimitsNormalizeForSystemd(t *testing.T) {
	for input, want := range map[string]string{"512MiB": "512M", "512MB": "512M", "512M": "512M", "1GiB": "1G", "512b": "512B", "1024": "1024"} {
		c := imageConfig()
		changeEnv(&c, func(e *config.Environment) { e.Quadlet.Memory = input })
		n, err := config.Normalize(c, "prod")
		if err != nil || n.Env.Quadlet.Memory != want {
			t.Fatalf("%s: %s %v", input, n.Env.Quadlet.Memory, err)
		}
		rendered, err := quadlet.Render(n, "")
		if err != nil || !strings.Contains(rendered, "MemoryMax="+want+"\n") {
			t.Fatal(rendered, err)
		}
	}
	for _, invalid := range []string{"0", "-1G", "1Ki", "10GiBB", "999999999999999999999G", "8E"} {
		if _, err := config.NormalizeMemory(invalid); err == nil {
			t.Fatalf("accepted invalid memory: %s", invalid)
		}
	}
}
