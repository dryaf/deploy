package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

const ServerInventory = `version: 2
servers:
  vps-main:
    host: vps.example.com
    user: deploy
    ssh_port: 2222
    ssh_key: keys/admin
    ssh_known_hosts: keys/known_hosts
  home-backup:
    host: raspberrypi
    user: backup
    backup_worker:
      sources: [vps-main]
      storage:
        path: /mnt/backup/deploy
        require_mount: /mnt/backup
      retention: {keep_last: 48, max_age: 720h}
      notifications:
        primary: signal
        signal:
          account_file: /home/backup/secrets/account
          recipient_file: /home/backup/secrets/recipient
`

func WriteFixture(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
