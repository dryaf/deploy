package config

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServerInventory deliberately does not merge global or parent inventories.
type ServerInventory struct {
	Version int                     `yaml:"version"`
	Servers map[string]ServerConfig `yaml:"servers"`
	Legacy  *ServerConfig           `yaml:"-"`
}

func ReadInventory(file string) (ServerInventory, error) {
	var inv ServerInventory
	data, err := os.ReadFile(file)
	if err != nil {
		return inv, err
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		return inv, fmt.Errorf("%s: invalid inventory YAML", file)
	}
	fields := yamlFields(&doc, map[*yaml.Node]bool{})
	if _, version := fields["version"]; !version {
		if _, servers := fields["servers"]; servers {
			return inv, fmt.Errorf("%s: servers requires version: 2", file)
		}
		cfg, err := ReadServer(file)
		if err != nil {
			return inv, err
		}
		if cfg.BackupWorker != nil {
			return inv, fmt.Errorf("backup_worker requires a version 2 inventory")
		}
		inv.Legacy = &cfg
		return inv, nil
	}
	if err = DecodeConfig(file, data, &inv); err != nil {
		return inv, err
	}
	if inv.Version != 2 || len(inv.Servers) == 0 {
		return inv, fmt.Errorf("%s: inventory requires version: 2 and nonempty servers", file)
	}
	rawServers := yamlFields(fields["servers"], map[*yaml.Node]bool{})
	for _, name := range sortedServerNames(inv.Servers) {
		cfg := inv.Servers[name]
		rawServer := yamlFields(rawServers[name], map[*yaml.Node]bool{})
		if !ValidInventoryName(name) {
			return inv, fmt.Errorf("invalid server name %q", name)
		}
		if !HostPattern.MatchString(cfg.Host) || !UserPattern.MatchString(cfg.User) {
			return inv, fmt.Errorf("server %s: explicit valid host and user required", name)
		}
		if cfg.SSHPort == 0 {
			if _, present := rawServer["ssh_port"]; present {
				return inv, fmt.Errorf("server %s: explicit ssh_port must be between 1 and 65535", name)
			}
			cfg.SSHPort = 22
		}
		if cfg.SSHPort < 1 || cfg.SSHPort > 65535 {
			return inv, fmt.Errorf("server %s: invalid ssh_port", name)
		}
		if err = validateSingleLines(reflect.ValueOf(cfg), "server."+name); err != nil {
			return inv, err
		}
		cfg.SSHKey, err = localInventoryPath(file, cfg.SSHKey)
		if err != nil {
			return inv, err
		}
		cfg.KnownHostsFile, err = localInventoryPath(file, cfg.KnownHostsFile)
		if err != nil {
			return inv, err
		}
		if cfg.BackupWorker != nil {
			workerFields := yamlFields(rawServer["backup_worker"], map[*yaml.Node]bool{})
			for _, numeric := range []struct {
				node  *yaml.Node
				value int64
				name  string
			}{
				{workerFields["max_snapshot_bytes"], cfg.BackupWorker.MaxSnapshotBytes, "max_snapshot_bytes"},
				{yamlFields(workerFields["storage"], map[*yaml.Node]bool{})["min_free_bytes"], cfg.BackupWorker.Storage.MinFreeBytes, "storage.min_free_bytes"},
			} {
				if numeric.node != nil && numeric.value <= 0 {
					return inv, fmt.Errorf("server %s: backup_worker.%s must be positive when supplied", name, numeric.name)
				}
			}
			retentionFields := yamlFields(workerFields["retention"], map[*yaml.Node]bool{})
			if _, present := retentionFields["keep_last"]; present && (cfg.BackupWorker.Retention == nil || cfg.BackupWorker.Retention.KeepLast <= 0) {
				return inv, fmt.Errorf("server %s: retention.keep_last must be positive", name)
			}
			if cfg.User == "root" {
				return inv, fmt.Errorf("server %s: backup worker must be rootless", name)
			}
			normalized, err := NormalizeWorker(*cfg.BackupWorker)
			if err != nil {
				return inv, fmt.Errorf("server %s: %w", name, err)
			}
			cfg.BackupWorker = &normalized
		}
		inv.Servers[name] = cfg
	}
	for name, cfg := range inv.Servers {
		if cfg.BackupWorker == nil {
			continue
		}
		seen := map[string]bool{}
		for _, source := range cfg.BackupWorker.Sources {
			s, ok := inv.Servers[source]
			if !ok {
				return inv, fmt.Errorf("worker %s: unknown source %s", name, source)
			}
			if s.User == "root" {
				return inv, fmt.Errorf("worker %s: source %s must be rootless", name, source)
			}
			key := serverAccount(s)
			if seen[key] {
				return inv, fmt.Errorf("worker %s: duplicate source account %s", name, source)
			}
			if key == serverAccount(cfg) {
				return inv, fmt.Errorf("worker %s: source must be a separate SSH account", name)
			}
			seen[key] = true
		}
	}
	return inv, nil
}

func ValidInventoryName(s string) bool {
	return aliasPattern.MatchString(s) && !strings.Contains(s, "..")
}

func sortedServerNames(m map[string]ServerConfig) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func serverAccount(s ServerConfig) string {
	return fmt.Sprintf("%s@%s:%d", s.User, strings.ToLower(strings.TrimSuffix(s.Host, ".")), SSHPort(ServerEnvironment(s)))
}

func ServerEnvironment(s ServerConfig) Environment {
	return Environment{Host: s.Host, User: s.User, Port: SSHPort(Environment{Port: s.SSHPort}), SSHKey: s.SSHKey, KnownHostsFile: s.KnownHostsFile}
}

func localInventoryPath(file, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~"))
	} else if strings.HasPrefix(value, "~") {
		return "", fmt.Errorf("SSH paths support only ~ or ~/ for the current operator")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(filepath.Dir(file), value)
	}
	return filepath.Abs(value)
}

func SelectServer(file, name string) (ServerConfig, error) {
	inv, err := ReadInventory(file)
	if err != nil {
		return ServerConfig{}, err
	}
	if inv.Legacy != nil {
		if name != "" {
			return ServerConfig{}, fmt.Errorf("legacy server file does not accept a name; migrate it first")
		}
		return *inv.Legacy, nil
	}
	if name == "" {
		return ServerConfig{}, fmt.Errorf("version 2 inventory requires an explicit server name")
	}
	s, ok := inv.Servers[name]
	if !ok {
		return s, fmt.Errorf("unknown server %q", name)
	}
	return s, nil
}

// Presence includes explicitly empty/null fields and YAML merge/alias fields.
func yamlFields(n *yaml.Node, seen map[*yaml.Node]bool) map[string]*yaml.Node {
	fields := map[string]*yaml.Node{}
	if n == nil || seen[n] {
		return fields
	}
	seen[n] = true
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range n.Content {
			for k, v := range yamlFields(child, seen) {
				fields[k] = v
			}
		}
	case yaml.AliasNode:
		return yamlFields(n.Alias, seen)
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "<<" {
				for k, v := range yamlFields(n.Content[i+1], seen) {
					fields[k] = v
				}
			}
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value != "<<" {
				fields[n.Content[i].Value] = n.Content[i+1]
			}
		}
	}
	return fields
}

func resolveServerReferences(cfg *Config, data []byte, file, inventoryFile string) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("invalid application YAML")
	}
	root := yamlFields(&doc, map[*yaml.Node]bool{})
	rawEnvs := yamlFields(root["environments"], map[*yaml.Node]bool{})
	var inv *ServerInventory
	for name, e := range cfg.Environments {
		fields := yamlFields(rawEnvs[name], map[*yaml.Node]bool{})
		if _, present := fields["server"]; !present {
			continue
		}
		if e.Server == "" {
			return fmt.Errorf("environment %s: server must name an inventory entry", name)
		}
		for _, key := range []string{"host", "user", "ssh_port", "ssh_key", "ssh_known_hosts"} {
			if _, present := fields[key]; present {
				return fmt.Errorf("environment %s: server cannot be combined with inline %s", name, key)
			}
		}
		if inv == nil {
			if inventoryFile == "" {
				inventoryFile = filepath.Join(filepath.Dir(file), "server.yaml")
			}
			loaded, err := ReadInventory(inventoryFile)
			if err != nil {
				return err
			}
			if loaded.Legacy != nil {
				return fmt.Errorf("environment %s: server references require version 2 inventory", name)
			}
			inv = &loaded
		}
		s, ok := inv.Servers[e.Server]
		if !ok {
			return fmt.Errorf("environment %s: unknown server %q", name, e.Server)
		}
		e.Host, e.User, e.Port, e.SSHKey, e.KnownHostsFile = s.Host, s.User, s.SSHPort, s.SSHKey, s.KnownHostsFile
		cfg.Environments[name] = e
	}
	return validateEnvironmentCollisions(*cfg)
}

func validateEnvironmentCollisions(cfg Config) error {
	services, targets, databases := map[string]string{}, map[string]string{}, map[string]string{}
	type binding struct {
		account, source, environment string
		readOnly                     bool
	}
	var bindings []binding
	names := make([]string, 0, len(cfg.Environments))
	for n := range cfg.Environments {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		e := cfg.Environments[name]
		account := serverAccount(ServerConfig{Host: e.Host, User: e.User, SSHPort: e.Port}) + "/"
		for _, item := range []struct {
			kind, value string
			seen        map[string]string
		}{
			{"service", e.Quadlet.ServiceName, services}, {"target directory", e.Dir, targets},
		} {
			if item.value == "" {
				continue
			}
			value := item.value
			if item.kind == "target directory" {
				value = path.Clean(value)
			}
			key := account + value
			if previous, exists := item.seen[key]; exists {
				return fmt.Errorf("environments %s and %s share %s on the same SSH account", previous, name, item.kind)
			}
			item.seen[key] = name
		}
		if e.Database.Source != "" {
			key := account + path.Join(e.Dir, e.Database.Source)
			if previous, exists := databases[key]; exists {
				return fmt.Errorf("environments %s and %s share a database binding", previous, name)
			}
			databases[key] = name
		}
		for _, volume := range e.Quadlet.Volumes {
			parts := strings.Split(volume, ":")
			if len(parts) < 2 {
				continue
			}
			source := parts[0]
			if strings.HasPrefix(source, "./") {
				source = path.Join(e.Dir, source)
			}
			if path.IsAbs(source) {
				source = path.Clean(source)
			}
			readOnly := len(parts) == 3 && containsVolumeOption(parts[2], "ro")
			for _, prior := range bindings {
				if prior.account == account && prior.environment != name && !(prior.readOnly && readOnly) && PathsOverlap(prior.source, source) {
					return fmt.Errorf("environments %s and %s share overlapping writable volume sources on the same SSH account", prior.environment, name)
				}
			}
			bindings = append(bindings, binding{account, source, name, readOnly})
		}
	}
	return nil
}

func containsVolumeOption(options, want string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == want {
			return true
		}
	}
	return false
}

// Migration is explicit and prints a candidate; it never rewrites the input.
func MigrateServer(file, name string, w io.Writer) error {
	if !ValidInventoryName(name) {
		return fmt.Errorf("invalid server name")
	}
	inv, err := ReadInventory(file)
	if err != nil {
		return err
	}
	if inv.Legacy == nil {
		return fmt.Errorf("inventory is already version 2")
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	defer enc.Close()
	return enc.Encode(ServerInventory{Version: 2, Servers: map[string]ServerConfig{name: *inv.Legacy}})
}
