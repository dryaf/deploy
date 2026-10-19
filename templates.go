package main

type TemplateData struct {
	ActivationGate string
	Quadlet
	TargetDir string
	Routing   bool
	EnvFile   string
	CPUQuota  string
	Pinned    bool
}

type MaintenanceTemplateData struct {
	ServiceName string
	Rule        string // Pre-calculated Traefik Rule
	Network     string
	TargetDir   string
	Resolver    string
}

type TraefikTemplateData struct {
	TraefikConfig
	HostUID string
}

const traefikContainerTmpl = `[Unit]
Description=Traefik Reverse Proxy
After=network-online.target
Wants=network-online.target

[Container]
Image=docker.io/library/traefik:{{ .Version }}
Network={{ if .NetworkName }}{{ .NetworkName }}{{ else }}traefik-net{{ end }}.network
PublishPort=80:80
PublishPort=443:443
Volume=/run/user/{{ .HostUID }}/podman/podman.sock:/var/run/docker.sock:Z
Volume=%h/traefik/traefik.yml:/etc/traefik/traefik.yml:ro,Z
Volume=%h/traefik/dynamic_conf:/etc/traefik/dynamic_conf:ro,Z
Volume=%h/traefik/letsencrypt:/letsencrypt:Z
Exec=--configfile=/etc/traefik/traefik.yml

[Install]
WantedBy=default.target
`

const traefikYmlTmpl = `api:
  dashboard: {{ .Dashboard }}

entryPoints:
  web:
    address: ":80"
    http:
      redirections:
        entryPoint:
          to: websecure
          scheme: https
  websecure:
    address: ":443"

certificatesResolvers:
  {{ .CertResolver }}:
    acme:
      email: "{{ .Email }}"
      storage: "/letsencrypt/acme.json"
      httpChallenge:
        entryPoint: web

providers:
  docker:
    endpoint: "unix:///var/run/docker.sock"
    exposedByDefault: false
  file:
    directory: "/etc/traefik/dynamic_conf"
    watch: true
`

const traefikDashboardTmpl = `http:
  routers:
    dashboard:
      rule: Host("traefik.localhost") || (PathPrefix("/api") && Headers("Referer", "traefik"))
      service: api@internal
      middlewares:
        - auth
  middlewares:
    auth:
      basicAuth:
        users:
          - "{{ .DashboardAuth }}"
`

const networkTmpl = `[Network]
Driver=bridge
`

const quadletTemplate = `[Unit]
Description={{ literal .Description }}
{{- if .Routing }}
Requires=traefik.service
After=traefik.service
{{- end }}
After=network-online.target
Wants=network-online.target

[Container]
Image={{ .Image }}
{{- if .Pinned }}
Pull=never
{{- end }}
{{- if .Exec }}
Exec={{ literal .Exec }}
{{- end }}
{{- if .Network }}
Network={{ literal .Network }}
{{- end }}
{{- range .NetworkAliases }}
NetworkAlias={{ literal . }}
{{- end }}
{{- if .Timezone }}
Timezone={{ quote .Timezone }}
{{- end }}
{{- if .ReadOnly }}
ReadOnly=true
{{- end }}
{{- if .HealthCmd }}
HealthCmd={{ quote .HealthCmd }}
HealthInterval=60s
HealthRetries=3
HealthTimeout=5s
{{- end }}
{{- range .Ports }}
PublishPort={{ literal . }}
{{- end }}
{{- range .Volumes }}
Volume={{ literal . }}
{{- end }}
{{- range .EnvVars }}
Environment={{ quote . }}
{{- end }}
{{- range .PodmanArgs }}
PodmanArgs={{ literal . }}
{{- end }}
{{- if .EnvFile }}
EnvironmentFile={{ quote .EnvFile }}
{{- end }}
{{- range .Labels }}
Label={{ quote . }}
{{- end }}

[Service]
{{- if .ActivationGate }}
ExecStartPre=/bin/bash {{ quote .ActivationGate }}
{{- end }}
{{- if .AutoRestart }}
Restart=on-failure
RestartSec=5s
{{- end }}
{{- if .CPUQuota }}
CPUQuota={{ .CPUQuota }}
{{- end }}
{{- if .Memory }}
MemoryMax={{ .Memory }}
{{- end }}

[Install]
WantedBy=default.target
`

// Updated: Uses {{.Rule}} and adds ReplacePathRegex middleware to handle deep links
const maintenanceContainerTmpl = `[Unit]
Description={{ .ServiceName }} Maintenance Page
Requires=traefik.service
After=network-online.target traefik.service

[Container]
Image=docker.io/library/nginx:alpine
Network={{ .Network }}
Volume={{ .TargetDir }}/maintenance/index.html:/usr/share/nginx/html/index.html:ro,Z

# --- Traefik Labels ---
Label="traefik.enable=true"

# 1. Router & Priority
Label="traefik.http.routers.{{ .ServiceName }}-maint.rule={{ .Rule }}"
Label="traefik.http.routers.{{ .ServiceName }}-maint.priority=1"
Label="traefik.http.routers.{{ .ServiceName }}-maint.entrypoints=websecure"
Label="traefik.http.routers.{{ .ServiceName }}-maint.tls.certresolver={{ .Resolver }}"

# 2. Middleware: Rewrite ALL paths to root (/) so Nginx serves index.html
Label="traefik.http.middlewares.{{ .ServiceName }}-maint-strip.replacepathregex.regex=^/.*"
Label="traefik.http.middlewares.{{ .ServiceName }}-maint-strip.replacepathregex.replacement=/"
Label="traefik.http.routers.{{ .ServiceName }}-maint.middlewares={{ .ServiceName }}-maint-strip"

# 3. Service
Label="traefik.http.services.{{ .ServiceName }}-maint.loadbalancer.server.port=80"

[Install]
WantedBy=default.target
`

const maintenanceHtmlTmpl = `<!doctype html>
<title>{{ .Title }}</title>
<style>
  body { text-align: center; padding: 150px; font: 20px Helvetica, sans-serif; color: #333; }
  h1 { font-size: 50px; margin-bottom: 10px; }
  article { display: block; text-align: left; width: 650px; margin: 0 auto; }
  a { color: #dc8100; text-decoration: none; }
  a:hover { color: #333; text-decoration: none; }
  .lang { border-top: 1px solid #eee; margin-top: 20px; padding-top: 20px; font-size: 0.8em; color: #777; }
</style>

<article>
    <h1>{{ .Title }}</h1>
    <div>
        <p>{{ .Text }}</p>
        <p>&mdash; The Team</p>
    </div>

    <div class="lang">
        <strong>Status:</strong> System Maintenance / Wartungsarbeiten<br>
        We will be back shortly. Wir sind gleich wieder da.
    </div>
</article>
`
