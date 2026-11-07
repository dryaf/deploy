package backup

import (
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

// Reference changes retain the bot. A new numeric chat additionally requires
// a settled outbox before review and under the installation publication lease.
// Both sets of private files remain external inputs. These are hashes/references,
// never secret values; the previous runtime and delivery journal stay intact.
type setupTelegramRotation struct {
	Version           int                   `json:"version"`
	BeforeRequest     string                `json:"before_request"`
	AfterRequest      string                `json:"after_request"`
	Before            config.WorkerTelegram `json:"before"`
	After             config.WorkerTelegram `json:"after"`
	Inputs            map[string]string     `json:"inputs"`
	BeforeDestination string                `json:"before_destination,omitempty"`
	AfterDestination  string                `json:"after_destination,omitempty"`
}

func changedBackupTelegram(before, after config.BackupWorkerConfig) bool {
	return !reflect.DeepEqual(before.Notifications.Telegram, after.Notifications.Telegram)
}

func validateBackupTelegramRotation(before WorkerInstallation, previous *SetupDeclaration, r WorkerSetupRequest) error {
	old, next := before.Policy, r.Policy
	b, a := old.Notifications.Telegram, next.Notifications.Telegram
	if b == nil || a == nil || old.Notifications.Mode != "" || next.Notifications.Mode != "" || old.Notifications.Primary != "telegram" || old.Notifications.Fallback != "" || old.Notifications.Signal != nil || *b == *a {
		return fmt.Errorf("backup credential rotation requires changed Telegram file references with unchanged bot identity and no fallback")
	}
	declaredBefore, declaredAfter := declaredSetupTelegram(previous), declaredSetupTelegram(r.Declaration)
	if declaredBefore == nil || declaredAfter == nil || *declaredBefore != *b || *declaredAfter != *a {
		return fmt.Errorf("backup credential rotation requires matching retained and proposed YAML snapshots")
	}
	next.Notifications.Telegram = b
	if !reflect.DeepEqual(next, old) || before.Image != r.Image || before.RelayImage != r.RelayImage || previous.HelperSHA256 != r.Declaration.HelperSHA256 || !reflect.DeepEqual(previous.Sources, r.Declaration.Sources) {
		return fmt.Errorf("apply Telegram reference changes separately from source, policy or artifact changes")
	}
	return nil
}

var backupTelegramTokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)
var backupTelegramChatPattern = regexp.MustCompile(`^-?[1-9][0-9]{0,19}$`)

func (v setupTelegramRotation) validate() error {
	if (v.Version != 1 && v.Version != 2 && v.Version != 3) || !safefs.DigestPattern.MatchString(v.BeforeRequest) || !safefs.DigestPattern.MatchString(v.AfterRequest) || v.BeforeRequest == v.AfterRequest || v.Before == v.After {
		return fmt.Errorf("invalid backup credential rotation authority")
	}
	if v.Version == 3 {
		if !safefs.DigestPattern.MatchString(v.BeforeDestination) || !safefs.DigestPattern.MatchString(v.AfterDestination) || v.BeforeDestination == v.AfterDestination || v.Before.ChatIDFile == v.After.ChatIDFile {
			return fmt.Errorf("destination migration requires distinct retained audience pins and files")
		}
	} else if v.BeforeDestination != "" || v.AfterDestination != "" {
		return fmt.Errorf("credential rotation cannot ignore destination migration authority")
	}
	// Version 1 has exactly the original token-only shape. Do not interpret a
	// downgraded reference-move record as authority to ignore its second chat.
	if v.Version == 1 && (v.Before.TokenFile == v.After.TokenFile || v.Before.ChatIDFile != v.After.ChatIDFile || len(v.Inputs) != 3) {
		return fmt.Errorf("invalid legacy backup credential rotation authority")
	}
	paths := v.paths()
	if len(v.Inputs) != len(paths) {
		return fmt.Errorf("backup credential rotation input set changed")
	}
	for p := range paths {
		if !config.WorkerAbsolutePath(p) || !safefs.DigestPattern.MatchString(v.Inputs[p]) {
			return fmt.Errorf("backup credential rotation input pins unavailable")
		}
	}
	if v.Before.TokenFile == v.Before.ChatIDFile || v.Before.TokenFile == v.After.ChatIDFile || v.After.TokenFile == v.Before.ChatIDFile || v.After.TokenFile == v.After.ChatIDFile {
		return fmt.Errorf("backup credential rotation inputs must be distinct")
	}
	return nil
}

func (v setupTelegramRotation) paths() map[string]bool {
	return map[string]bool{v.Before.TokenFile: true, v.After.TokenFile: true, v.Before.ChatIDFile: true, v.After.ChatIDFile: true}
}

func prepareSetupTelegramRotation(j *WorkerSetupJournal, before WorkerInstallation, r WorkerSetupRequest) error {
	if !changedBackupTelegram(before.Policy, r.Policy) {
		if j.TelegramRotation != nil {
			return fmt.Errorf("pending credential rotation cannot be retargeted")
		}
		return nil
	}
	if err := validateBackupTelegramRotation(before, j.Declaration, r); err != nil {
		return err
	}
	v := setupTelegramRotation{Version: 2, BeforeRequest: j.RequestHash, AfterRequest: workerSetupRequestHash(r), Before: *before.Policy.Notifications.Telegram, After: *r.Policy.Notifications.Telegram, Inputs: map[string]string{}}
	values := map[string]string{}
	for p := range v.paths() {
		value, err := notification.ReadWorkerSecret(p)
		if err != nil {
			return err
		}
		values[p], v.Inputs[p] = value, sharedSetupDigest([]byte(value))
	}
	if err := v.validate(); err != nil {
		return err
	}
	oldToken, nextToken := values[v.Before.TokenFile], values[v.After.TokenFile]
	oldBot, _, _ := strings.Cut(oldToken, ":")
	nextBot, _, _ := strings.Cut(nextToken, ":")
	if !backupTelegramTokenPattern.MatchString(oldToken) || !backupTelegramTokenPattern.MatchString(nextToken) || oldBot != nextBot {
		return fmt.Errorf("backup reference changes must preserve the Telegram bot")
	}
	beforeDestination := provider.TelegramIdentity(oldToken, values[v.Before.ChatIDFile])
	afterDestination := provider.TelegramIdentity(nextToken, values[v.After.ChatIDFile])
	if beforeDestination != afterDestination {
		if !backupTelegramChatPattern.MatchString(values[v.After.ChatIDFile]) {
			return fmt.Errorf("select a private file containing the new numeric Telegram chat ID")
		}
		v.Version, v.BeforeDestination, v.AfterDestination = 3, beforeDestination, afterDestination
		if err := v.validate(); err != nil {
			return err
		}
		state, err := openInstalledBackupState(j.Declaration.Home, false)
		if err != nil {
			return err
		}
		err = requireSettledBackupDestination(state)
		state.Close()
		if err != nil {
			return err
		}
	}
	j.TelegramRotation = &v
	return nil
}

func checkSetupTelegramRotation(j WorkerSetupJournal, request *WorkerSetupRequest, rollback bool) error {
	previous, next := declaredSetupTelegram(j.PreviousDeclaration), declaredSetupTelegram(j.Declaration)
	if j.TelegramRotation == nil {
		if j.Updating && previous != nil && next != nil && *previous != *next {
			return fmt.Errorf("backup credential rotation evidence is missing; recovery required")
		}
		return nil
	}
	v := *j.TelegramRotation
	if err := v.validate(); err != nil {
		return err
	}
	if !j.Updating || j.PreviousDeclaration == nil || j.Declaration == nil || j.PreviousRequestHash != v.BeforeRequest || j.RequestHash != v.AfterRequest || j.Producer != nil || j.Shared != nil {
		return fmt.Errorf("backup credential rotation lost its predecessor or delivery authority")
	}
	if previous == nil || next == nil || *previous != v.Before || *next != v.After {
		return fmt.Errorf("backup credential rotation differs from retained declarations")
	}
	if request != nil && (workerSetupRequestHash(*request) != v.AfterRequest || request.Policy.Notifications.Telegram == nil || *request.Policy.Notifications.Telegram != v.After) {
		return fmt.Errorf("resume credential rotation with the same declaration and artifacts")
	}
	for path, pin := range v.Inputs {
		if rollback && path != v.Before.TokenFile && path != v.Before.ChatIDFile {
			continue
		}
		value, err := notification.ReadWorkerSecret(path)
		if err != nil || sharedSetupDigest([]byte(value)) != pin {
			return fmt.Errorf("retained backup credential changed; restore the reviewed private files before resuming")
		}
	}
	if v.Version == 3 {
		oldToken, e := notification.ReadWorkerSecret(v.Before.TokenFile)
		if e != nil {
			return e
		}
		oldChat, e := notification.ReadWorkerSecret(v.Before.ChatIDFile)
		if e != nil {
			return e
		}
		if provider.TelegramIdentity(oldToken, oldChat) != v.BeforeDestination {
			return fmt.Errorf("retained Telegram destination changed")
		}
		if !rollback {
			nextToken, e := notification.ReadWorkerSecret(v.After.TokenFile)
			if e != nil {
				return e
			}
			nextChat, e := notification.ReadWorkerSecret(v.After.ChatIDFile)
			if e != nil {
				return e
			}
			oldBot, _, _ := strings.Cut(oldToken, ":")
			nextBot, _, _ := strings.Cut(nextToken, ":")
			if oldBot != nextBot || !backupTelegramChatPattern.MatchString(nextChat) || provider.TelegramIdentity(nextToken, nextChat) != v.AfterDestination {
				return fmt.Errorf("reviewed Telegram destination changed")
			}
		}
	}
	return nil
}

func declaredSetupTelegram(d *SetupDeclaration) *config.WorkerTelegram {
	if d == nil || d.Desired == nil || d.Desired.Backup == nil || d.Desired.Backup.Notifications.Telegram == nil {
		return nil
	}
	p := *d.Desired.Backup.Notifications.Telegram
	for _, ref := range []*string{&p.TokenFile, &p.ChatIDFile} {
		if strings.HasPrefix(*ref, "~/") {
			*ref = filepath.Join(d.Home, strings.TrimPrefix(*ref, "~/"))
		}
	}
	return &p
}
