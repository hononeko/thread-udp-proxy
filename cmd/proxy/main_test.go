package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hononeko/thread-udp-proxy/pkg/config"
	"github.com/hononeko/thread-udp-proxy/pkg/proxy"
)

func newTestMux(t *testing.T) (*http.ServeMux, *state) {
	t.Helper()
	cfg, err := config.Load(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	st := &state{}
	p := proxy.New(proxy.Config{Port: cfg.Port, ThreadInterface: cfg.ThreadInterface, IdleTimeout: cfg.IdleTimeout})
	return setupMux(cfg, p, st), st
}

func get(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	mux, _ := newTestMux(t)
	// Live even before the interface is attached, so a missing interface doesn't restart the container.
	if rec := get(mux, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", rec.Code)
	}
}

func TestReadyzFollowsInterfaceState(t *testing.T) {
	mux, st := newTestMux(t)

	if rec := get(mux, "/readyz"); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "starting") {
		t.Fatalf("/readyz before binding = %d %q, want 503 starting", rec.Code, rec.Body)
	}

	st.set(false, errors.New(`failed to find interface "net1"`))
	if rec := get(mux, "/readyz"); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "net1") {
		t.Fatalf("/readyz after a failure = %d %q, want 503 with the error", rec.Code, rec.Body)
	}

	st.set(true, nil)
	if rec := get(mux, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("/readyz when bound = %d, want 200", rec.Code)
	}
}

func TestStatus(t *testing.T) {
	mux, st := newTestMux(t)
	st.set(true, nil)

	rec := get(mux, "/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("/status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/status is not JSON: %v", err)
	}
	if body["interface"] != "net1" || body["thread_interface"] != "wpan0" || body["port"] != float64(5540) || body["bound"] != true {
		t.Fatalf("/status = %v", body)
	}
}

func TestMetrics(t *testing.T) {
	mux, st := newTestMux(t)
	st.set(true, nil)

	body := get(mux, "/metrics").Body.String()
	for _, want := range []string{
		"thread_udp_proxy_up 1",
		"thread_udp_proxy_forwarded_datagrams_total 0",
		"thread_udp_proxy_reply_datagrams_total 0",
		"thread_udp_proxy_dropped_datagrams_total 0",
		"thread_udp_proxy_ignored_frames_total 0",
		"thread_udp_proxy_active_clients 0",
		"# TYPE thread_udp_proxy_forward_latency_seconds histogram",
		`thread_udp_proxy_forward_latency_seconds_bucket{le="1e-05"} 0`,
		`thread_udp_proxy_forward_latency_seconds_bucket{le="0.0025"} 0`,
		`thread_udp_proxy_forward_latency_seconds_bucket{le="+Inf"} 0`,
		"thread_udp_proxy_forward_latency_seconds_sum 0",
		"thread_udp_proxy_forward_latency_seconds_count 0",
		"# TYPE thread_udp_proxy_reply_latency_seconds histogram",
		`thread_udp_proxy_reply_latency_seconds_bucket{le="0.1"} 0`,
		"thread_udp_proxy_reply_latency_seconds_count 0",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("/metrics is missing %q", want)
		}
	}
}

func TestRejectsOtherMethods(t *testing.T) {
	mux, _ := newTestMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz = %d, want 405", rec.Code)
	}
}

func TestCLIHealthCheck(t *testing.T) {
	mux, _ := newTestMux(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	if code := runCLIHealthCheck(host, port); code != 0 {
		t.Fatalf("health check against a live server = %d, want 0", code)
	}
	srv.Close()
	if code := runCLIHealthCheck(host, port); code != 1 {
		t.Fatalf("health check against a stopped server = %d, want 1", code)
	}
}
