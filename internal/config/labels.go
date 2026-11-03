package config

import (
	"fmt"
	"sort"
	"strings"
)

func TraefikLabels(serviceName string, r RouterConfig, defaultResolver string) []string {
	var labels []string
	if r.Enabled != nil && !*r.Enabled {
		return []string{"traefik.enable=false"}
	}
	if r.Domain == "" && r.Host == "" && r.Rule == "" {
		return labels
	}

	labels = append(labels, "traefik.enable=true")

	// High Priority for Main App (beats maintenance page)
	labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.priority=100", serviceName))

	rule := r.Rule
	if rule == "" {
		if r.Domain != "" {
			rule = fmt.Sprintf("Host(`%s`)", r.Domain)
		} else {
			rule = fmt.Sprintf("Host(`%s`)", r.Host)
		}
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

	// New Simplified Auth Flag
	if r.Auth {
		// Convention: We assume the server has a middleware named "global-auth"
		// This is created by 'deploy server provision'
		mws = append(mws, "global-auth")
	}

	// Legacy/Advanced Auth Headers
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
		keys := make([]string, 0, len(r.Headers))
		for k := range r.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := r.Headers[k]
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
