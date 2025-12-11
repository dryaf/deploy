package main

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseDeployConfig(t *testing.T) {
	yamlData := `
app_name: "my-app"
binary_name: "server"
build:
  arch: "amd64"
  ldflags: "-X main.ver=1.0"
artifacts:
  include: ["static"]
environments:
  prod:
    host: "10.0.0.1"
    user: "admin"
    ssh_port: 2222
    target_dir: "/app"
    quadlet:
      service_name: "my-app-service"
      router:
        domain: "app.example.com"
        auth: true
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(yamlData), &cfg); err != nil {
		t.Fatalf("Failed to parse config: %v", err)
	}

	if cfg.AppName != "my-app" {
		t.Errorf("Expected AppName 'my-app', got '%s'", cfg.AppName)
	}
	if cfg.Environments["prod"].Host != "10.0.0.1" {
		t.Errorf("Expected Host '10.0.0.1', got '%s'", cfg.Environments["prod"].Host)
	}
	if !cfg.Environments["prod"].Quadlet.Router.Auth {
		t.Errorf("Expected Router.Auth to be true")
	}
}

func TestParseServerConfig(t *testing.T) {
	yamlData := `
host: "1.2.3.4"
user: "root"
ssh_port: 22
stack:
  traefik:
    version: "v3"
    auth:
      provider: "authelia"
  authelia:
    subdomain: "auth"
`
	var cfg ServerConfig
	if err := yaml.Unmarshal([]byte(yamlData), &cfg); err != nil {
		t.Fatalf("Failed to parse server config: %v", err)
	}

	if cfg.Host != "1.2.3.4" {
		t.Errorf("Expected Host '1.2.3.4', got '%s'", cfg.Host)
	}
	if cfg.Stack.Traefik.Auth.Provider != "authelia" {
		t.Errorf("Expected Auth Provider 'authelia', got '%s'", cfg.Stack.Traefik.Auth.Provider)
	}
	if cfg.Stack.Authelia.Subdomain != "auth" {
		t.Errorf("Expected Authelia Subdomain 'auth', got '%s'", cfg.Stack.Authelia.Subdomain)
	}
}
