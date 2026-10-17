package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRightsOwnership(t *testing.T) {
	for _, target := range []string{"user", "container"} {
		t.Run(target, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			log := filepath.Join(dir, "podman-args")
			for name, script := range map[string]string{
				"ssh":    "#!/bin/bash\nexec bash -c \"${!#}\"\n",
				"podman": "#!/bin/bash\nprintf '%s\\0' \"$@\" > \"$ARG_LOG\"\n",
				"id":     "#!/bin/bash\ncase $1 in -u) echo 1000;; -g) echo 1001;; *) exit 1;; esac\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			t.Setenv("ARG_LOG", log)
			project := "/srv/project with ' quotes $literal"
			shared := "/srv/shared uploads/$(false)"
			c := imageConfig()
			c.Deployment.Mode = ModeBinary
			c.BinaryName = "server"
			changeEnv(&c, func(e *Environment) {
				e.Dir = project
				e.Quadlet.ContainerUID = 65532
				e.Quadlet.ContainerGID = 65533
				e.Quadlet.ChownVolumes = []string{"./data", shared}
			})
			body, err := yaml.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile("deploy.yaml", body, 0600); err != nil {
				t.Fatal(err)
			}
			doRights("prod", target)
			args, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			owner := "0:0"
			if target == "container" {
				owner = "65532:65533"
			}
			want := []string{"unshare", "chown", "-R", owner, "--", project + "/data", shared}
			if got := strings.Split(strings.TrimSuffix(string(args), "\x00"), "\x00"); !reflect.DeepEqual(got, want) {
				t.Fatalf("ownership arguments: got %q, want %q", got, want)
			}
		})
	}
}
