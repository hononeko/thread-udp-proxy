package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hononeko/thread-udp-proxy/pkg/config"
	"github.com/hononeko/thread-udp-proxy/pkg/logger"
	"github.com/hononeko/thread-udp-proxy/pkg/proxy"
	"github.com/hononeko/thread-udp-proxy/pkg/version"
)

const (
	// readTimeout bounds how long a raw socket read blocks, i.e. how quickly the proxy notices shutdown.
	readTimeout = time.Second
	// retryDelay is the wait before reopening the interface after it was missing or failed.
	retryDelay = 5 * time.Second
)

// state tracks whether the proxy is attached to its interface, for the readiness probe.
type state struct {
	mu      sync.RWMutex
	bound   bool
	lastErr string
}

func (s *state) set(bound bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bound = bound
	s.lastErr = ""
	if err != nil {
		s.lastErr = err.Error()
	}
}

func (s *state) get() (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bound, s.lastErr
}

func main() {
	for _, arg := range os.Args[1:] {
		switch arg {
		case "-version", "--version":
			fmt.Printf("thread-udp-proxy %s\n", version.Version)
			return
		case "-healthcheck", "--healthcheck":
			os.Exit(runCLIHealthCheck(os.Getenv("LISTEN_ADDR"), os.Getenv("LISTEN_PORT")))
		}
	}

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		logger.Fatal("Invalid configuration", "err", err)
	}
	logger.InitWithOptions(cfg.Log, os.Stdout)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	prefixes := make([]string, len(cfg.Prefixes))
	for i, p := range cfg.Prefixes {
		prefixes[i] = p.String()
	}
	logger.Info("Starting thread-udp-proxy", "version", version.Version, "interface", cfg.Interface,
		"port", cfg.Port, "prefixes", strings.Join(prefixes, ","), "idleTimeout", cfg.IdleTimeout)

	p := proxy.New(proxy.Config{Port: cfg.Port, Prefixes: cfg.Prefixes, IdleTimeout: cfg.IdleTimeout})
	st := &state{}
	// Attach before serving, so the readiness probe is accurate from its first answer.
	conn, err := proxy.Listen(cfg.Interface, readTimeout)
	st.set(err == nil, err)

	server := &http.Server{
		Addr:              net.JoinHostPort(cfg.ListenAddr, cfg.ListenPort),
		Handler:           setupMux(cfg, p, st),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
	}
	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()
	logger.Info("Health server listening", "addr", server.Addr)

	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		supervise(ctx, cfg.Interface, p, st, conn, err)
	}()

	select {
	case <-ctx.Done():
		logger.Info("Shutdown signal received, shutting down gracefully...")
	case err := <-serverErr:
		logger.Fatal("HTTP server failed unexpectedly", "err", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("Server shutdown failed", "err", err)
	}
	<-proxyDone
	logger.Info("Stopped cleanly")
}

// supervise relays on conn (or retries opening iface after err), reopening the interface whenever it's missing or
// fails, until ctx is cancelled. The interface may appear after the process starts, so failing to open it isn't fatal:
// readiness reports it.
func supervise(ctx context.Context, iface string, p *proxy.Proxy, st *state, conn proxy.FrameConn, err error) {
	for {
		if err == nil {
			st.set(true, nil)
			logger.Info("Relaying", "interface", iface)
			err = p.Run(ctx, conn)
			_ = conn.Close()
		}
		if ctx.Err() != nil {
			return
		}
		st.set(false, err)
		logger.Error("Proxy not running, retrying", "interface", iface, "in", retryDelay, "err", err)

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
		conn, err = proxy.Listen(iface, readTimeout)
	}
}

func setupMux(cfg config.Config, p *proxy.Proxy, st *state) *http.ServeMux {
	mux := http.NewServeMux()

	// /healthz - Liveness probe: the process is serving.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	})

	// /readyz - Readiness probe: the proxy is attached to its interface.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if bound, lastErr := st.get(); !bound {
			w.WriteHeader(http.StatusServiceUnavailable)
			if lastErr == "" {
				lastErr = "starting"
			}
			_, _ = fmt.Fprintf(w, "NOT READY: %s", lastErr)
			return
		}
		_, _ = w.Write([]byte("READY"))
	})

	// /status - JSON diagnostic endpoint.
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		bound, lastErr := st.get()
		stats := p.Stats()
		prefixes := make([]string, len(cfg.Prefixes))
		for i, prefix := range cfg.Prefixes {
			prefixes[i] = prefix.String()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":        version.Version,
			"interface":      cfg.Interface,
			"port":           cfg.Port,
			"prefixes":       prefixes,
			"idle_timeout":   cfg.IdleTimeout.String(),
			"bound":          bound,
			"last_error":     lastErr,
			"forwarded":      stats.Forwarded,
			"replies":        stats.Replies,
			"dropped":        stats.Dropped,
			"ignored":        stats.Ignored,
			"active_clients": stats.ActiveMappings,
		})
	})

	// /metrics - Prometheus exposition endpoint.
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		bound, _ := st.get()
		stats := p.Stats()
		up := 0
		if bound {
			up = 1
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = fmt.Fprintf(w, `# HELP thread_udp_proxy_up Whether the proxy is attached to its interface.
# TYPE thread_udp_proxy_up gauge
thread_udp_proxy_up %d
# HELP thread_udp_proxy_forwarded_datagrams_total Datagrams relayed from clients into the Thread mesh.
# TYPE thread_udp_proxy_forwarded_datagrams_total counter
thread_udp_proxy_forwarded_datagrams_total %d
# HELP thread_udp_proxy_reply_datagrams_total Replies relayed from the Thread mesh back to clients.
# TYPE thread_udp_proxy_reply_datagrams_total counter
thread_udp_proxy_reply_datagrams_total %d
# HELP thread_udp_proxy_dropped_datagrams_total Datagrams that matched but could not be relayed.
# TYPE thread_udp_proxy_dropped_datagrams_total counter
thread_udp_proxy_dropped_datagrams_total %d
# HELP thread_udp_proxy_ignored_frames_total Frames read from the interface that weren't ours to relay.
# TYPE thread_udp_proxy_ignored_frames_total counter
thread_udp_proxy_ignored_frames_total %d
# HELP thread_udp_proxy_active_clients Clients with an open relay socket.
# TYPE thread_udp_proxy_active_clients gauge
thread_udp_proxy_active_clients %d
`, up, stats.Forwarded, stats.Replies, stats.Dropped, stats.Ignored, stats.ActiveMappings)
		writeHistogram(w, "thread_udp_proxy_forward_latency_seconds",
			"Time from a client frame reaching the interface to its datagram being sent into the mesh.", stats.ForwardLatency)
		writeHistogram(w, "thread_udp_proxy_reply_latency_seconds",
			"Time from a reply reaching its socket to its frame being written back to the client.", stats.ReplyLatency)
	})

	return mux
}

// writeHistogram writes h in the Prometheus text exposition format.
func writeHistogram(w io.Writer, name, help string, h proxy.Histogram) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	for i, bound := range proxy.LatencyBuckets {
		_, _ = fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", name, strconv.FormatFloat(bound.Seconds(), 'g', -1, 64), h.Cumulative[i])
	}
	_, _ = fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %s\n%s_count %d\n",
		name, h.Count, name, strconv.FormatFloat(h.Sum.Seconds(), 'g', -1, 64), name, h.Count)
}

func runCLIHealthCheck(listenAddr, listenPort string) int {
	host := listenAddr
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if listenPort == "" {
		listenPort = config.DefaultListenPort
	}
	targetURL := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, listenPort))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The URL is this process's own health endpoint, built from the operator's LISTEN_ADDR/LISTEN_PORT.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil) //nolint:gosec // see above
	if err != nil {
		fmt.Fprintf(os.Stderr, "Health check error creating request: %v\n", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // same local health endpoint
	if err != nil {
		fmt.Fprintf(os.Stderr, "Health check failed: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Health check returned status: %d\n", resp.StatusCode)
		return 1
	}
	fmt.Println("Health check OK")
	return 0
}
