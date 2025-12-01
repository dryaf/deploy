package main

import (
	"fmt"
	"os"
	"strings"
)

func doTraefikSetup(envName string) {
	_, env := loadEnv(envName)
	if env.Traefik.Email == "" {
		logFatal("Traefik email missing in deploy.yaml")
	}

	version := env.Traefik.Version
	if version == "" || version == "latest" {
		logInfo("🔍 Checking GitHub for latest Traefik version...")
		if v, err := fetchLatestGitHubRelease("traefik/traefik"); err == nil {
			version = v
			logInfo("Latest version: %s", version)
		} else {
			version = "v3.0"
			logWarn("GitHub check failed. Defaulting to %s", version)
		}
	}
	env.Traefik.Version = version

	logInfo("🚀 Configuring Traefik on %s...", env.Host)

	sshArgs := getSSHBaseArgs(env)
	sshArgs = append(sshArgs, "id -u")
	uidStr := getCmdOutput("ssh", sshArgs...)
	if uidStr == "" {
		logFatal("Cannot determine remote UID")
	}

	if !dryRun {
		os.MkdirAll("build/traefik", 0755)
	}
	tmplData := TraefikTemplateData{env.Traefik, uidStr}

	netName := env.Traefik.NetworkName
	if netName == "" {
		netName = "traefik-net"
	}

	genFile("build/traefik/traefik.yml", traefikYmlTmpl, tmplData)
	genFile("build/traefik/traefik.container", strings.Replace(traefikContainerTmpl, "traefik-net", netName, -1), tmplData)
	genFile("build/traefik/"+netName+".network", networkTmpl, nil)

	if env.Traefik.Dashboard && env.Traefik.DashboardAuth != "" {
		if !dryRun {
			os.MkdirAll("build/traefik/dynamic_conf", 0755)
		}
		genFile("build/traefik/dynamic_conf/dashboard.yml", traefikDashboardTmpl, tmplData)
	}

	logInfo("📂 Setting up remote directories & permissions...")
	runSSH(env, "mkdir -p ~/traefik/dynamic_conf ~/traefik/letsencrypt ~/.config/containers/systemd")
	runSSH(env, "touch ~/traefik/letsencrypt/acme.json && chmod 600 ~/traefik/letsencrypt/acme.json")

	logInfo("📤 Syncing configs...")
	runRsync(env, []string{"build/traefik/traefik.yml"}, fmt.Sprintf("%s@%s:~/traefik/", env.User, env.Host))
	if env.Traefik.DashboardAuth != "" {
		runRsync(env, []string{"build/traefik/dynamic_conf/"}, fmt.Sprintf("%s@%s:~/traefik/dynamic_conf/", env.User, env.Host))
	}
	runRsync(env, []string{"build/traefik/traefik.container", "build/traefik/" + netName + ".network"},
		fmt.Sprintf("%s@%s:~/.config/containers/systemd/", env.User, env.Host))

	logInfo("🔄 Starting Traefik...")
	script := strings.Join([]string{
		"systemctl --user daemon-reload",
		"systemctl --user restart traefik.service",
		"sleep 2",
		"systemctl --user is-active traefik.service",
	}, " && ")

	if err := runSSH(env, script); err != nil {
		logFatal("Traefik failed to start. Check 'deploy logs traefik'")
	}
	logSuccess("✅ Traefik deployed successfully.")
}

func generateTraefikLabels(serviceName string, r RouterConfig, defaultResolver string) []string {
	var labels []string
	if r.Host == "" && r.Rule == "" {
		return labels
	}

	labels = append(labels, "traefik.enable=true")

	// High Priority for Main App (beats maintenance page)
	labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.priority=100", serviceName))

	rule := r.Rule
	if rule == "" {
		rule = fmt.Sprintf("Host(`%s`)", r.Host)
	}
	labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.rule=%s", serviceName, rule))

	eps := r.EntryPoints
	if len(eps) == 0 {
		eps = []string{"websecure"}
	}
	labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.entrypoints=%s", serviceName, strings.Join(eps, ",")))

	resolver := r.CertResolver
	if resolver == "" {
		resolver = defaultResolver
	}
	if resolver == "" {
		resolver = "myresolver"
	}
	labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.tls.certresolver=%s", serviceName, resolver))

	var mws []string
	if r.StripPrefix && r.PathPrefix != "" {
		mw := serviceName + "-strip"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.stripprefix.prefixes=%s", mw, r.PathPrefix))
		mws = append(mws, mw)
	}
	if len(r.BasicAuth) > 0 {
		mw := serviceName + "-auth"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.basicauth.users=%s", mw, strings.Join(r.BasicAuth, ",")))
		mws = append(mws, mw)
	}
	if r.BasicAuthFile != "" {
		mw := serviceName + "-authfile"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.basicauth.usersfile=%s", mw, r.BasicAuthFile))
		mws = append(mws, mw)
	}
	if len(r.IPAllowList) > 0 {
		mw := serviceName + "-ip"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.ipallowlist.sourcerange=%s", mw, strings.Join(r.IPAllowList, ",")))
		mws = append(mws, mw)
	}
	if r.RateLimit != nil && r.RateLimit.Average > 0 {
		mw := serviceName + "-rate"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.ratelimit.average=%d", mw, r.RateLimit.Average))
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.ratelimit.burst=%d", mw, r.RateLimit.Burst))
		mws = append(mws, mw)
	}
	if r.Compress {
		mw := serviceName + "-compress"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.compress=true", mw))
		mws = append(mws, mw)
	}
	if len(r.Headers) > 0 {
		mw := serviceName + "-headers"
		for k, v := range r.Headers {
			labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.headers.customrequestheaders.%s=%s", mw, k, v))
		}
		mws = append(mws, mw)
	}
	if len(mws) > 0 {
		labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.middlewares=%s", serviceName, strings.Join(mws, ",")))
	}

	port := r.InternalPort
	if port == 0 {
		port = 8080
	}
	labels = append(labels, fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port=%d", serviceName, port))
	return labels
}
