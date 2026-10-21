# Agent & Contributor Guidelines (`AGENTS.md`)

This document covers the architecture principles, development workflow, quality gates and best practices for developing and maintaining `thread-udp-proxy`.

All automated agents and human contributors must follow the rules in this document.

---

## 1. Non-Negotiable Pre-Commit Quality Gates

Before committing or submitting changes, all of the following steps **MUST** pass cleanly:

1. **Code Formatting (`gofmt`):**
   ```bash
   gofmt -s -w .
   ```
   `git diff` must show zero formatting changes afterwards.

2. **Test Suite Must Pass**, with the race detector:
   ```bash
   go test -v -race ./...
   ```

3. **Static Analysis & Linting** (`errcheck`, `govet`, `staticcheck`, `gosec`, `unused`, `dupl`):
   ```bash
   golangci-lint run
   ```
   Only use `//nolint` together with an inline comment justifying it.

4. **Dependency Hygiene:**
   ```bash
   go mod tidy -diff
   ```

---

## 2. Golang Engineering Standards

### 2.1 Context & Lifecycle Management
* **Propagate contexts:** every long-running loop or background task takes a `context.Context` and exits when it's cancelled.
* **Graceful termination:** `signal.NotifyContext` on `os.Interrupt` and `SIGTERM`. The HTTP server and the proxy shut down within a bounded timeout.
* **Bounded blocking:** raw socket reads time out (`ErrTimeout`) so readers notice cancellation. Never add a read that can block forever.

### 2.2 Packet Handling
* **The data path stays allocation-light, single-owner and hand-off-free:** each datagram is relayed by the goroutine that read it, because waking another goroutine costs more than the relay itself on an idle host. The goroutine in `proxy.Run` reads frames, forwards them and alone owns the client mappings map, sweeping it between reads. Each client's reply goroutine writes its replies to the `FrameConn` itself, sharing only the fields on `mapping` that are atomic. Counters are atomics too. `Run` waits for every reply goroutine before returning, so its caller can close the `FrameConn` safely.
* **Validate before trusting:** `packet.Parse` checks every length field before slicing. Malformed or unrelated frames are ignored silently, because the interface carries plenty of traffic that isn't ours.
* **Never relay our own frames:** the raw socket only accepts `PACKET_HOST` frames, i.e. frames addressed to this host. Keep it that way, or replies can loop.
* **Filter in the kernel, check in the proxy:** the raw socket carries a classic BPF filter (`relayFilter`) that only passes UDP over IPv6 to the relayed port, and ignores outgoing frames (`PACKET_IGNORE_OUTGOING`). This only saves work. `ReadFrame` and `handleFrame` keep every check, so the proxy stays correct without the filter. A filter change needs a case in `filter_test.go`, which runs the program in `x/net/bpf`'s VM.
* **Checksums:** IPv6 UDP checksums are mandatory. `packet.Build` always writes one, never `0`.

### 2.3 Error Handling & Logging
* **Wrap errors:** `fmt.Errorf("failed to ...: %w", err)`.
* **Never swallow errors silently:** handle them with a fallback and a log line, count them in the `dropped` counter, or return them.
* **Structured logging:** use `pkg/logger` (`charmbracelet/log`, interoperable with `log/slog`) with key-value pairs. Per-datagram logs are `Debug` only. `Info` is for lifecycle events: start, attach, new or expired client.

### 2.4 Testing Best Practices
* **Deterministic tests:** never wait with a bare `time.Sleep`. Use channels, or polling loops with a deadline.
* **No privileges in tests:** the proxy is tested through the `FrameConn` interface with a fake connection, and a loopback UDP server stands in for a Thread node. Code that needs `CAP_NET_RAW` stays in `rawsock_linux.go`, and the image smoke test covers it.

---

## 3. Container & Docker Best Practices

### 3.1 Minimal & Rootless Containers
* **Static binary:** `CGO_ENABLED=0 go build -trimpath -ldflags="-w -s"`.
* **Distroless base:** `gcr.io/distroless/static-debian12:nonroot`.
* **Non-root execution:** `USER 65532:65532`, distroless's `nonroot`. The UID must be numeric, or Kubernetes can't verify `runAsNonRoot`. `CAP_NET_RAW` is granted as a file capability (`setcap cap_net_raw=+ep`), never by running as root.

### 3.2 HTTP Server Hardening
* Every `http.Server` sets `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` and `MaxHeaderBytes`.
* `LISTEN_ADDR` allows binding to `127.0.0.1` in a shared pod network namespace.

### 3.3 Probing & Health Checks
* `/healthz`: liveness. It must not depend on the interface, so a missing interface never restarts the container.
* `/readyz`: readiness, meaning attached to `PROXY_INTERFACE`. Main attaches before serving, so the first answer is accurate.
* `-healthcheck`: native CLI probe for runtimes without `curl`.

---

## 4. Repository Structure & Conventions

```
.
├── cmd/
│   └── proxy/              # Entrypoint: config, health/metrics server, interface supervision
├── pkg/
│   ├── config/             # Environment configuration and validation
│   ├── logger/             # Structured logging (shared with the other hononeko sidecars)
│   ├── packet/             # Ethernet/IPv6/UDP frame parsing and building, checksums
│   ├── proxy/              # Relay engine; rawsock_linux.go is the AF_PACKET FrameConn
│   └── version/            # Build version, set at link time
├── .golangci.yml           # Linter configuration
├── Dockerfile              # Multi-stage rootless distroless build
└── AGENTS.md               # Repository rules & contributor guidelines
```

### Commit Guidelines
Follow the [Conventional Commits](https://www.conventionalcommits.org/) specification. Commit subjects become the release notes, so write them for a reader of the release:
* `feat:` a new feature or capability
* `fix:` a bug fix or stability correction
* `refactor:` code restructuring without behavioral changes
* `test:` adding or improving tests
* `docs:` documentation updates
* `chore:` maintenance, dependency bumps, tooling updates

---

## 5. Release Pipeline

Versions and release notes come from the commit history, using the rules shared by all my repositories in [`hononeko/actions`](https://github.com/hononeko/actions):
* A `!` in the header (`feat!:`) or a `BREAKING CHANGE:` footer gives a **major** release.
* `feat:` gives a **minor** release.
* `docs:` and `test:` give no release.
* Every other type gives a **patch** release.

1. **Pull requests (`pr.yml`):** lint and tests run. Then the image is built and smoke-tested natively on `linux/amd64` and `linux/arm64` (`image.yml`). The smoke test attaches to the container's `eth0` and requires `/readyz` to return 200, which proves the nonroot binary can open its raw socket.

   **Snapshots:** a pull request from a branch of this repository labelled `snapshot` also publishes its head commit as a multi-arch image tagged `pr-<PR>-sha-<commit>` and `pr-<PR>` (the newest), after every push, and a comment on the pull request names it. Snapshots are for testing in the cluster before merging, never for the gitops repository. `snapshot-cleanup.yml` deletes them when the pull request closes.
2. **Every push to `main` (`main.yml`):** the same checks run. If there's something to release, both architectures are published as one attested multi-arch **release candidate**: `X.Y.Z-rc.N`, `sha-<commit>` and `edge`.
3. **Stable release (`cd.yml`, dispatched manually from `main`):**
   * promotes the dispatched commit's release candidate by re-tagging it as `X.Y.Z` and `latest` (same digest, no rebuild);
   * creates the git tag and the GitHub Release, with notes grouped into 💥 Breaking changes, 🚀 Features, 🐛 Bug fixes, 👷 CI and 📦 Others;
   * prunes that version's other release candidates from GHCR.

   Dispatch with `dry_run` to see the release notes and check the candidate without changing anything.
