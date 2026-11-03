package notification

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"

	"github.com/dryaf/deploy/internal/config"
)

// Compare the check addressed by a URL, never the filename containing its secret.
// Do not include either URL in errors: the path/query may contain credentials.
func monitoringCheckIdentity(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("monitoring check requires a valid HTTPS URL")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	port := u.Port()
	if port != "" && port != "443" {
		host = net.JoinHostPort(host, port)
	}
	p := path.Clean("/" + strings.TrimLeft(u.Path, "/"))
	// Status endpoints refer to the same check, not independent monitors.
	for _, suffix := range []string{"/fail", "/start", "/success"} {
		p = strings.TrimSuffix(p, suffix)
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid monitoring check query")
	}
	return host + "/" + strings.Trim(p, "/") + "?" + query.Encode(), nil
}

func ValidateMonitoringChecks(m config.WorkerMonitoring) error {
	identities := map[string]bool{}
	for _, file := range []string{m.BackupPingURLFile, m.DailyDeliveryPingURLFile} {
		if file == "" {
			continue
		}
		secret, err := ReadWorkerSecret(file)
		if err != nil {
			return err
		}
		id, err := monitoringCheckIdentity(secret)
		if err != nil {
			return err
		}
		if identities[id] {
			return fmt.Errorf("backup and delivery monitoring URLs address the same check")
		}
		identities[id] = true
	}
	return nil
}
