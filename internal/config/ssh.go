package config

func SSHPort(env Environment) int {
	if env.Port == 0 {
		return 22
	}
	return env.Port
}
