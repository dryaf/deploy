package main

import (
	"fmt"
	"strings"
	"testing"
)

type provisioningRunner struct{ fakeRunner }

func (r *provisioningRunner) SSH(e Environment, script string) (string, error) {
	_, err := r.fakeRunner.SSH(e, script)
	if err != nil {
		return "", err
	}
	if script == "id -u" {
		return "1000\n", nil
	}
	return "", nil
}

func TestProvisioningPropagatesFailures(t *testing.T) {
	cfg := ServerConfig{Host: "example.com", User: "deploy"}
	cfg.Stack.Traefik.Version = "v3.0"
	for failure := 1; failure <= 5; failure++ {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			t.Chdir(t.TempDir())
			runner := &provisioningRunner{fakeRunner{failAt: failure}}
			if err := provisionServer(cfg, runner); err == nil {
				t.Fatal("provisioning reported success")
			}
			if len(runner.calls) != failure {
				t.Fatal("continued after failure", runner.calls)
			}
		})
	}
	t.Run("successful", func(t *testing.T) {
		t.Chdir(t.TempDir())
		runner := &provisioningRunner{}
		if err := provisionServer(cfg, runner); err != nil {
			t.Fatal(err)
		}
		for _, call := range runner.calls {
			if strings.HasPrefix(call, "sync ") && strings.Contains(call, ":~/") {
				t.Fatal(call)
			}
		}
	})
	t.Run("unsupported provider fails before SSH", func(t *testing.T) {
		cfg.Stack.Traefik.Auth.Provider = "authelia"
		runner := &provisioningRunner{}
		if err := provisionServer(cfg, runner); err == nil || len(runner.calls) != 0 {
			t.Fatal(err, runner.calls)
		}
	})
}
