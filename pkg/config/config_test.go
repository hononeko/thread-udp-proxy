package config

import (
	"net/netip"
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
	if cfg.Interface != "net1" || cfg.Port != 5540 || cfg.IdleTimeout != 5*time.Minute {
		t.Errorf("proxy defaults = %q, %d, %s; want net1, 5540, 5m", cfg.Interface, cfg.Port, cfg.IdleTimeout)
	}
	if want := []netip.Prefix{netip.MustParsePrefix("fc00::/7")}; !slices.Equal(cfg.Prefixes, want) {
		t.Errorf("Prefixes = %v, want %v", cfg.Prefixes, want)
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
		"PROXY_INTERFACE":    "eth1",
		"PROXY_PORT":         "5683",
		"PROXY_PREFIXES":     " fd2f:741e:f59b:1::1/64 , fdde:ad00:beef:0::/64,",
		"PROXY_IDLE_TIMEOUT": "90s",
		"LISTEN_ADDR":        "127.0.0.1",
		"LISTEN_PORT":        "8081",
		"LOG_LEVEL":          "debug",
		"LOG_CALLER":         "true",
		"LOG_TIMESTAMP":      "false",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Interface != "eth1" || cfg.Port != 5683 || cfg.IdleTimeout != 90*time.Second {
		t.Errorf("proxy settings = %q, %d, %s", cfg.Interface, cfg.Port, cfg.IdleTimeout)
	}
	// Prefixes are masked, so a host address in the setting still means its whole network.
	want := []netip.Prefix{netip.MustParsePrefix("fd2f:741e:f59b:1::/64"), netip.MustParsePrefix("fdde:ad00:beef::/64")}
	if !slices.Equal(cfg.Prefixes, want) {
		t.Errorf("Prefixes = %v, want %v", cfg.Prefixes, want)
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
		{"PROXY_PORT", "0", "PROXY_PORT"},
		{"PROXY_PORT", "70000", "PROXY_PORT"},
		{"PROXY_PORT", "matter", "PROXY_PORT"},
		{"PROXY_PREFIXES", "fd00::/129", "invalid prefix"},
		{"PROXY_PREFIXES", "192.168.4.0/24", "not an IPv6 prefix"},
		{"PROXY_PREFIXES", "::ffff:192.168.4.0/120", "not an IPv6 prefix"},
		{"PROXY_PREFIXES", " , ", "at least one prefix"},
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
