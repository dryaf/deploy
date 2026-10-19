package main

import (
	"fmt"
	"path"
	"reflect"
	"time"
)

type TimeoutConfig struct {
	Connect       string `yaml:"connect"`
	Transfer      string `yaml:"transfer"`
	Build         string `yaml:"build"`
	Pull          string `yaml:"pull"`
	Database      string `yaml:"database"`
	Service       string `yaml:"service"`
	Recovery      string `yaml:"recovery"`
	ShutdownGrace string `yaml:"shutdown_grace"`
}

type RetentionConfig struct {
	KeepLast int    `yaml:"keep_last" json:"keep_last"`
	MaxAge   string `yaml:"max_age" json:"max_age"`
}

type StorageConfig struct {
	MinFreeBytes      int64           `yaml:"min_free_bytes"`
	MinFreeInodes     int64           `yaml:"min_free_inodes"`
	ImageReserveBytes int64           `yaml:"image_reserve_bytes"`
	Releases          RetentionConfig `yaml:"releases"`
}

// Destination is an absolute directory on an explicitly configured SSH host.
type OffHostConfig struct {
	Host         string `yaml:"host"`
	User         string `yaml:"user"`
	Port         int    `yaml:"ssh_port"`
	Key          string `yaml:"ssh_key"`
	Dir          string `yaml:"target_dir"`
	RestoreCheck string `yaml:"restore_check"`
}

var defaultTimeouts = TimeoutConfig{Connect: "10s", Transfer: "10m", Build: "15m", Pull: "15m", Database: "5m", Service: "60s", Recovery: "5m", ShutdownGrace: "10s"}

func normalizeTimeouts(value TimeoutConfig) (TimeoutConfig, error) {
	v, d := reflect.ValueOf(&value).Elem(), reflect.ValueOf(defaultTimeouts)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).String() == "" {
			v.Field(i).SetString(d.Field(i).String())
		}
		duration, err := time.ParseDuration(v.Field(i).String())
		if err != nil || duration <= 0 || duration > 24*time.Hour {
			return value, fmt.Errorf("timeouts.%s must be a positive duration of at most 24h", v.Type().Field(i).Tag.Get("yaml"))
		}
	}
	return value, nil
}

func budget(value, fallback string) time.Duration {
	if value == "" {
		value = fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		d, _ = time.ParseDuration(fallback)
	}
	return d
}

func validateRetention(name string, r RetentionConfig) error {
	if r.KeepLast < 0 {
		return fmt.Errorf("%s.keep_last must be nonnegative", name)
	}
	if r.MaxAge != "" {
		d, err := time.ParseDuration(r.MaxAge)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s.max_age must be a positive duration", name)
		}
	}
	return nil
}

func validateOffHost(o OffHostConfig) error {
	if o == (OffHostConfig{}) {
		return nil
	}
	if !hostPattern.MatchString(o.Host) || !userPattern.MatchString(o.User) || !path.IsAbs(o.Dir) || path.Clean(o.Dir) == "/" || o.Port < 0 || o.Port > 65535 {
		return fmt.Errorf("database.off_host requires a valid host, user, port and absolute target_dir")
	}
	return nil
}
