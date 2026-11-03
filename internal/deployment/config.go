package deployment

import (
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/ui"
)

func readConfig(file string) (config.Config, error) {
	return config.Read(file, ui.ServersFile)
}

func loadConfig() config.Config {
	cfg, err := readConfig("deploy.yaml")
	if err != nil {
		ui.LogFatal("Config: %v", err)
	}
	return cfg
}

func loadServerConfig() config.ServerConfig {
	cfg, err := config.ReadServer("server.yaml")
	if err != nil {
		ui.LogFatal("Config: %v", err)
	}
	return cfg
}

func ValidateConfiguration(env string, server bool) error {
	if server {
		_, err := config.ReadInventory(ui.InventoryPath(""))
		return err
	}
	cfg, err := readConfig("deploy.yaml")
	if err != nil {
		return err
	}
	n, err := config.Normalize(cfg, env)
	if err != nil {
		return err
	}
	_, err = quadlet.Render(n, "")
	return err
}

func LoadEnv(envName string) (config.Config, config.Environment) {
	cfg := loadConfig()
	n, err := config.Normalize(cfg, envName)
	if err != nil {
		ui.LogFatal("Config: %v", err)
	}
	return n.Config, n.Env
}
