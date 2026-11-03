package config

type WorkerRestoreCheck struct {
	Source   string   `yaml:"source" json:"source"`
	Instance string   `yaml:"instance" json:"instance"`
	Image    string   `yaml:"image" json:"image"`
	Command  []string `yaml:"command" json:"command"`
	Timeout  string   `yaml:"timeout" json:"timeout"`
}
