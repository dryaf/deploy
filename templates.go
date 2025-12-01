package main

type TemplateData struct {
	Quadlet
	TargetDir string
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
Description={{ if .Description }}{{ .Description }}{{ else }}{{ .ServiceName }} Service{{ end }}
Requires=traefik.service
After=network-online.target traefik.service
Wants=network-online.target

[Container]
Image={{ .Image }}
{{- if .Exec }}
Exec={{ .Exec }}
{{- end }}
{{- if .Network }}
Network={{ .Network }}
{{- end }}
{{- if .Timezone }}
Timezone={{ .Timezone }}
{{- end }}
{{- if .Memory }}
Memory={{ .Memory }}
{{- end }}
{{- if .CPU }}
CPUQuota={{ .CPU }}
{{- end }}
{{- if .ReadOnly }}
ReadOnly=true
{{- end }}
{{- if .HealthCmd }}
HealthCmd={{ .HealthCmd }}
HealthInterval=60s
HealthRetries=3
{{- end }}
{{- range .Ports }}
PublishPort={{ . }}
{{- end }}
{{- range .Volumes }}
Volume={{ . }}
{{- end }}
{{- range .EnvVars }}
Environment={{ . }}
{{- end }}
{{- range .PodmanArgs }}
PodmanArgs={{ . }}
{{- end }}
EnvironmentFile={{ .TargetDir }}/.env
{{- range .Labels }}
Label="{{ . }}"
{{- end }}

[Install]
WantedBy=default.target
`
