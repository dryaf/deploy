package main

import (
	"strings"
	"testing"
)

func TestGenerateTraefikLabels(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		router      RouterConfig
		wantLabels  []string
	}{
		{
			name:        "Simple Auth Host",
			serviceName: "app",
			router: RouterConfig{
				Domain: "app.com",
				Auth:   true,
			},
			wantLabels: []string{
				"traefik.enable=true",
				"traefik.http.routers.app.rule=Host(`app.com`)",
				"traefik.http.routers.app.middlewares=global-auth",
			},
		},
		{
			name:        "Complex Legacy",
			serviceName: "legacy",
			router: RouterConfig{
				Host:         "old.com",
				InternalPort: 3000,
				PathPrefix:   "/api",
				StripPrefix:  true,
			},
			wantLabels: []string{
				"traefik.http.routers.legacy.rule=Host(`old.com`)",
				"traefik.http.middlewares.legacy-strip.stripprefix.prefixes=/api",
				"traefik.http.services.legacy.loadbalancer.server.port=3000",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := generateTraefikLabels(tt.serviceName, tt.router, "resolver")
			for _, want := range tt.wantLabels {
				found := false
				for _, g := range got {
					if strings.Contains(g, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Missing expected label: %s. Got: %v", want, got)
				}
			}
		})
	}
}
