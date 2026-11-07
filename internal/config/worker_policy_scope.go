package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// NotificationPolicyScope separates gateway policy from specialist-only policy.
// A backup schedule/retention edit does not change the gateway's desired runtime,
// while freshness, route and consumer enablement do. The scope version is part
// of the digest; existing whole-inventory pins require an audited conversion.
// This is not permission to drop other fields: the backup coordinator continues
// to retain and reconcile its complete declaration independently.
type NotificationPolicyScope struct {
	Version      int                      `json:"version"`
	Notification *NotificationDeclaration `json:"notification_worker"`
	Backup       *NotificationBackupScope `json:"backup,omitempty"`
}
type NotificationBackupScope struct {
	Worker    string `json:"worker"`
	Route     string `json:"route"`
	Freshness string `json:"freshness,omitempty"`
}

func (d DesiredWorkers) NotificationScope() NotificationPolicyScope {
	s := NotificationPolicyScope{Version: 1, Notification: d.Notification}
	b := d.Backup
	if b != nil && WorkerEnabled(b.Enabled) && WorkerEnabled(b.Notifications.Enabled) && b.Notifications.Worker != "" {
		s.Backup = &NotificationBackupScope{Worker: b.Notifications.Worker, Route: b.Notifications.Route}
		// The default producer derives its evidence bound from backup freshness.
		// An explicitly declared producer supplies that bound in notification policy.
		if d.Notification == nil {
			s.Backup.Freshness = b.Freshness
		} else if _, explicit := d.Notification.Producers["backup-worker"]; !explicit {
			s.Backup.Freshness = b.Freshness
		}
	}
	return s
}

func (d DesiredWorkers) NotificationFingerprint() (string, error) {
	raw, err := json.Marshal(d.NotificationScope())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}
