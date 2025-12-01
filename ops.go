package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func doGenAuth(user, password string) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		logFatal("Hash generation failed: %v", err)
	}
	fmt.Printf("%s:%s\n", user, string(hash))
}

func doPrune(envName string) {
	_, env := loadEnv(envName)
	logInfo("🧹 Pruning unused resources on %s (%s)...", envName, env.Host)

	logInfo("   - Pruning dangling images...")
	if err := runSSH(env, "podman image prune -f"); err != nil {
		logWarn("Image prune warning: %v", err)
	}

	logInfo("   - Pruning build cache...")
	if err := runSSH(env, "podman builder prune -f"); err != nil {
		logWarn("Builder prune warning: %v", err)
	}

	logSuccess("✅ Prune complete.")
}

func doRights(envName, target string) {
	_, env := loadEnv(envName)
	if len(env.Quadlet.ChownVolumes) == 0 {
		logWarn("No 'chown_volumes' configured for this environment.")
		return
	}

	var uid, gid string
	if target == "user" {
		logInfo("🔧 Reclaiming ownership for SSH User...")
		uid = "$(id -u)"
		gid = "$(id -g)"
	} else if target == "container" {
		logInfo("🔧 Setting ownership for Container (%d:%d)...", env.Quadlet.ContainerUID, env.Quadlet.ContainerGID)
		if env.Quadlet.ContainerUID == 0 {
			logFatal("container_uid not set in config")
		}
		uid = fmt.Sprintf("%d", env.Quadlet.ContainerUID)
		gid = fmt.Sprintf("%d", env.Quadlet.ContainerGID)
	} else {
		logFatal("Invalid target. Use 'user' or 'container'")
	}

	changeOwnership(env, uid, gid)
	logSuccess("Permissions updated.")
}

func changeOwnership(env Environment, uid, gid string) {
	var paths []string
	for _, p := range env.Quadlet.ChownVolumes {
		if strings.HasPrefix(p, "./") {
			p = fmt.Sprintf("%s/%s", strings.TrimRight(env.Dir, "/"), strings.TrimPrefix(p, "./"))
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return
	}

	cmd := fmt.Sprintf("podman unshare chown -R %s:%s %s", uid, gid, strings.Join(paths, " "))
	runSSH(env, cmd)
}

func doLogs(envName string, usePodman bool) {
	_, env := loadEnv(envName)
	cmd := fmt.Sprintf("journalctl --user -u %s.service -f", env.Quadlet.ServiceName)
	if usePodman {
		cmd = fmt.Sprintf("podman logs -f systemd-%s", env.Quadlet.ServiceName)
	}
	logInfo("Streaming logs...")

	sshArgs := getSSHBaseArgs(env)
	sshArgs = append(sshArgs, "-t", cmd)

	c := exec.Command("ssh", sshArgs...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	c.Run()
}

func doServiceAction(envName, action string) {
	_, env := loadEnv(envName)
	serviceName := env.Quadlet.ServiceName

	valid := map[string]bool{
		"start":   true,
		"stop":    true,
		"restart": true,
		"enable":  true,
		"disable": true,
	}
	if !valid[action] {
		logFatal("Invalid action '%s'. Use start, stop, restart, enable, or disable.", action)
	}

	logInfo("⚙️  Executing '%s' on service '%s' (%s)...", action, serviceName, env.Host)

	cmd := fmt.Sprintf("systemctl --user %s %s.service", action, serviceName)
	if err := runSSH(env, cmd); err != nil {
		logFatal("Action '%s' failed: %v", action, err)
	}

	if action == "start" || action == "restart" {
		time.Sleep(2 * time.Second)
		logInfo("Checking status...")
		runSSHStream(env, fmt.Sprintf("systemctl --user is-active %s.service", serviceName))
	}

	logSuccess("Service action '%s' completed.", action)
}

func doInit() {
	if _, err := os.Stat("deploy.yaml"); err == nil {
		logFatal("deploy.yaml exists")
	}
	os.WriteFile("deploy.yaml", []byte(getDefaultConfig()), 0644)
	logSuccess("Created deploy.yaml")
}

func getDefaultConfig() string {
	return `app_name: "my-app"
binary_name: "server"

build:
  arch: "amd64"
  ldflags: "-s -w -X 'main.Version={{.Version}}' -X 'main.Commit={{.Commit}}'"

artifacts:
  include: ["migrations/", "Dockerfile"]
  exclude: ["data/", "*.db"]

environments:
  prod:
    host: "vps.example.com"
    user: "deploy_user"
    ssh_port: 22
    # ssh_key: "~/.ssh/id_ed25519_vps" # Optional
    target_dir: "/home/deploy_user/web/my-app"
    sync_env_file: ".env"

    traefik:
      email: "admin@example.com"
      network_name: "traefik-net"

    quadlet:
      service_name: "my-app"
      image: "localhost/my-app:latest"
      network: "traefik-net.network"
      auto_restart: true
      timezone: "Europe/Vienna"

      container_uid: 65532
      container_gid: 65532
      chown_volumes: ["./data"]

      volumes:
        - "./data:/data:Z"

      router:
        host: "app.example.com"
        internal_port: 8080
        https_redirect: true

      # health_url: "http://localhost:8080/health"

      env_vars:
        - "APP_ENV=production"
`
}
