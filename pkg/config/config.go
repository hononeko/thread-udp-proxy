// Package config loads the proxy's settings from environment variables.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hononeko/thread-udp-proxy/pkg/logger"
)

// Defaults match an OpenThread Border Router pod with a Multus interface named net1 relaying Matter traffic.
const (
	DefaultInterface       = "net1"
	DefaultThreadInterface = "wpan0"
	DefaultPort            = 5540
	DefaultIdleTimeout     = 5 * time.Minute
	DefaultListenPort      = "9090"
)

// Config is the complete runtime configuration.
type Config struct {
	// Interface is where clients' frames arrive and replies leave (PROXY_INTERFACE).
	Interface string
	// ThreadInterface is the interface into the Thread mesh, which client sockets are bound to (PROXY_THREAD_INTERFACE).
	ThreadInterface string
	// Port is the UDP destination port to relay (PROXY_PORT).
	Port uint16
	// IdleTimeout closes a client's socket after this long without traffic (PROXY_IDLE_TIMEOUT).
	IdleTimeout time.Duration

	// ListenAddr and ListenPort are where the health and metrics server binds (LISTEN_ADDR, LISTEN_PORT).
	ListenAddr string
	ListenPort string

	Log logger.Options

	// Obsolete lists the settings that are set but no longer do anything, so they can be warned about.
	Obsolete []string
}

// obsolete are settings that no longer do anything. Setting one isn't an error, so deployments that still do keep
// starting. PROXY_PREFIXES made the proxy refuse traffic clients route to it, and PROXY_THREAD_INTERFACE now keeps it
// in the mesh instead.
var obsolete = []string{"PROXY_PREFIXES"}

// Load reads the configuration through lookup (os.LookupEnv in production). Invalid proxy settings are errors
// rather than silent fallbacks: relaying the wrong traffic is worse than not starting.
func Load(lookup func(string) (string, bool)) (Config, error) {
	get := func(key, fallback string) string {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return fallback
	}

	cfg := Config{
		Interface:       get("PROXY_INTERFACE", DefaultInterface),
		ThreadInterface: get("PROXY_THREAD_INTERFACE", DefaultThreadInterface),
		ListenAddr:      get("LISTEN_ADDR", ""),
		ListenPort:      get("LISTEN_PORT", DefaultListenPort),
		Log: logger.Options{
			Level:      get("LOG_LEVEL", "info"),
			Format:     get("LOG_FORMAT", "text"),
			TimeFormat: get("LOG_TIME_FORMAT", ""),
			Color:      get("LOG_COLOR", ""),
		},
	}

	// Bound to the interface clients' frames arrive on, the proxy would send their datagrams straight back out of it.
	if cfg.ThreadInterface == cfg.Interface {
		return Config{}, fmt.Errorf("PROXY_THREAD_INTERFACE must be the Thread interface, not PROXY_INTERFACE (%q)", cfg.Interface)
	}

	port, err := strconv.ParseUint(get("PROXY_PORT", strconv.Itoa(DefaultPort)), 10, 16)
	if err != nil || port == 0 {
		return Config{}, fmt.Errorf("PROXY_PORT must be a port between 1 and 65535, got %q", get("PROXY_PORT", ""))
	}
	cfg.Port = uint16(port)

	cfg.IdleTimeout, err = time.ParseDuration(get("PROXY_IDLE_TIMEOUT", DefaultIdleTimeout.String()))
	if err != nil || cfg.IdleTimeout <= 0 {
		return Config{}, fmt.Errorf("PROXY_IDLE_TIMEOUT must be a positive duration such as 5m, got %q", get("PROXY_IDLE_TIMEOUT", ""))
	}

	if cfg.Log.ReportCaller, err = parseBool(get("LOG_CALLER", "false")); err != nil {
		return Config{}, fmt.Errorf("LOG_CALLER: %w", err)
	}
	if cfg.Log.ReportTimestamp, err = parseBool(get("LOG_TIMESTAMP", "true")); err != nil {
		return Config{}, fmt.Errorf("LOG_TIMESTAMP: %w", err)
	}

	for _, key := range obsolete {
		if get(key, "") != "" {
			cfg.Obsolete = append(cfg.Obsolete, key)
		}
	}
	return cfg, nil
}

func parseBool(s string) (bool, error) {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("must be true or false, got %q", s)
	}
	return b, nil
}
