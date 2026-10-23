package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Interface != "net1" || cfg.ThreadInterface != "wpan0" || cfg.Port != 5540 || cfg.IdleTimeout != 5*time.Minute {
		t.Errorf("proxy defaults = %q, %q, %d, %s; want net1, wpan0, 5540, 5m", cfg.Interface, cfg.ThreadInterface, cfg.Port, cfg.IdleTimeout)
	}
	if len(cfg.Obsolete) != 0 {
		t.Errorf("Obsolete = %v, want none", cfg.Obsolete)
	}
	if cfg.ListenAddr != "" || cfg.ListenPort != "9090" {
		t.Errorf("listen = %q:%q, want :9090", cfg.ListenAddr, cfg.ListenPort)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "text" || !cfg.Log.ReportTimestamp || cfg.Log.ReportCaller {
		t.Errorf("log defaults = %+v", cfg.Log)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"PROXY_INTERFACE":        "eth1",
		"PROXY_THREAD_INTERFACE": "wpan1",
		"PROXY_PORT":             "5683",
		"PROXY_IDLE_TIMEOUT":     "90s",
		"LISTEN_ADDR":            "127.0.0.1",
		"LISTEN_PORT":            "8081",
		"LOG_LEVEL":              "debug",
		"LOG_CALLER":             "true",
		"LOG_TIMESTAMP":          "false",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Interface != "eth1" || cfg.ThreadInterface != "wpan1" || cfg.Port != 5683 || cfg.IdleTimeout != 90*time.Second {
		t.Errorf("proxy settings = %q, %q, %d, %s", cfg.Interface, cfg.ThreadInterface, cfg.Port, cfg.IdleTimeout)
	}
	if cfg.ListenAddr != "127.0.0.1" || cfg.ListenPort != "8081" {
		t.Errorf("listen = %q:%q", cfg.ListenAddr, cfg.ListenPort)
	}
	if cfg.Log.Level != "debug" || !cfg.Log.ReportCaller || cfg.Log.ReportTimestamp {
		t.Errorf("log = %+v", cfg.Log)
	}
}

func TestLoadRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		key, value, wantErr string
	}{
		{"PROXY_THREAD_INTERFACE", "net1", "PROXY_THREAD_INTERFACE"},
		{"PROXY_PORT", "0", "PROXY_PORT"},
		{"PROXY_PORT", "70000", "PROXY_PORT"},
		{"PROXY_PORT", "matter", "PROXY_PORT"},
		{"PROXY_IDLE_TIMEOUT", "5", "PROXY_IDLE_TIMEOUT"},
		{"PROXY_IDLE_TIMEOUT", "-1m", "PROXY_IDLE_TIMEOUT"},
		{"LOG_CALLER", "maybe", "LOG_CALLER"},
		{"LOG_TIMESTAMP", "sometimes", "LOG_TIMESTAMP"},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			_, err := Load(env(map[string]string{tt.key: tt.value}))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load error = %v, want one mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadReportsObsoleteSettings(t *testing.T) {
	// Still accepted, even with a value it used to reject, so a deployment that sets it keeps starting.
	cfg, err := Load(env(map[string]string{"PROXY_PREFIXES": "fd2f:741e:f59b:1::/64,not a prefix"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"PROXY_PREFIXES"}; !slices.Equal(cfg.Obsolete, want) {
		t.Errorf("Obsolete = %v, want %v", cfg.Obsolete, want)
	}

	// Set but blank, as when a template renders an empty value: as good as unset.
	if cfg, err = Load(env(map[string]string{"PROXY_PREFIXES": " "})); err != nil || len(cfg.Obsolete) != 0 {
		t.Errorf("blank PROXY_PREFIXES: Obsolete = %v, err = %v; want none", cfg.Obsolete, err)
	}
}
