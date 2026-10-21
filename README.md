# thread-udp-proxy

A tiny UDP relay that lets a client on an infrastructure network (e.g. [matter-server](https://github.com/matter-js/matterjs-server)) reach devices in a Thread mesh through a specific [OpenThread Border Router](https://openthread.io/guides/border-router) (OTBR), and get their replies back the same way.

It runs as a sidecar next to `otbr-agent`, sharing its network namespace. It's a static Go binary in a distroless, nonroot image of about 18 MB that uses about 3 MiB of memory.

## Why

When several border routers advertise the same Thread prefix, you don't choose which one a client uses, and replies from the mesh may come back through a different border router than the request went out on. This proxy pins both directions to one OTBR:

1. The client routes traffic for the Thread prefix to the OTBR pod. That route can be static, or learned from the OTBR's router advertisements (e.g. `ot-ctl br rioprf high`).
2. The proxy picks up those frames on the infrastructure interface and sends each payload into the mesh from a socket of its own, one per client. To the Thread node, the request comes from the OTBR.
3. The node's reply comes back to that socket. The proxy writes it to the client as a raw frame that appears to come straight from the node.

A BPF filter on the raw socket passes only UDP over IPv6 to the relayed port, so the rest of the interface's traffic (neighbor discovery, mDNS, the host's own frames) is dropped in the kernel and costs the proxy nothing.

This is NAT, so the end-to-end payload is untouched. Protocols that don't bind sessions to IP addresses, like Matter, which secures messages by session ID, keep working.

```
matter-server ──(dst fd2f:…:5540, via OTBR MAC)──▶ net1 ┐
                                                         │ thread-udp-proxy
matter-server ◀──(raw frame, src = Thread node)──── net1 ┘   │  one UDP socket per client
                                                             ▼
                                                  wpan0 ──▶ Thread node
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `PROXY_INTERFACE` | `net1` | Interface where client frames arrive and replies leave |
| `PROXY_PORT` | `5540` | UDP destination port to relay (Matter) |
| `PROXY_PREFIXES` | `fc00::/7` | Comma-separated IPv6 prefixes to relay, i.e. the Thread mesh's |
| `PROXY_IDLE_TIMEOUT` | `5m` | Close a client's socket after this long without traffic |
| `LISTEN_ADDR` | *(all)* | Bind address of the health and metrics server |
| `LISTEN_PORT` | `9090` | Port of the health and metrics server |
| `LOG_LEVEL` | `info` | `debug` logs every relayed datagram |
| `LOG_FORMAT` | `text` | `text`, `json` or `logfmt` |
| `LOG_CALLER`, `LOG_TIMESTAMP`, `LOG_TIME_FORMAT`, `LOG_COLOR` | | As in the other hononeko sidecars |

The proxy fails to start on an invalid setting instead of falling back to a default.

## Endpoints

| Path | Description |
|---|---|
| `/healthz` | Liveness: the process is serving |
| `/readyz` | Readiness: attached to `PROXY_INTERFACE`. The proxy keeps retrying every 5s while the interface is missing |
| `/status` | JSON: settings, attachment state, last error, counters |
| `/metrics` | Prometheus: `thread_udp_proxy_up`, `_forwarded_datagrams_total`, `_reply_datagrams_total`, `_dropped_datagrams_total`, `_ignored_frames_total`, `_active_clients`, and the `_forward_latency_seconds` and `_reply_latency_seconds` histograms |

The latency histograms measure the time each datagram spends in the proxy, from the kernel's receive timestamp to the send that relays it, from 10µs up to 100ms. `_ignored_frames_total` counts frames the proxy read from the interface but didn't relay, such as other traffic addressed to the host.

The binary also accepts `-healthcheck` (probes `/healthz`, for runtimes without `curl`) and `-version`.

## Kubernetes

The proxy needs **`CAP_NET_RAW`**. The image grants it to the binary as a file capability, so it runs as the `nonroot` user (UID 65532), but the container must keep `NET_RAW` in its capability set. **Without it the binary can't even be executed** (`exec ...: operation not permitted`).

```yaml
- name: thread-udp-proxy
  image: ghcr.io/hononeko/thread-udp-proxy:X.Y.Z
  env:
    - name: PROXY_PREFIXES
      value: fd2f:741e:f59b:1::/64
  ports:
    - name: health
      containerPort: 9090
  livenessProbe:
    httpGet: { path: /healthz, port: health }
  readinessProbe:
    httpGet: { path: /readyz, port: health }
  securityContext:
    runAsNonRoot: true
    readOnlyRootFilesystem: true
    allowPrivilegeEscalation: false
    capabilities:
      drop: ["ALL"]
      add: ["NET_RAW"]
  resources:
    requests: { cpu: 10m, memory: 16Mi }
    limits: { cpu: 100m, memory: 32Mi }
```

It must share the network namespace with `otbr-agent`, as any container in the same pod does, so it sees the infrastructure interface and `wpan0`.

## Limitations

- **IPv6 UDP only**, carried directly in the IPv6 header. Packets with extension headers aren't relayed.
- **Untagged Ethernet only**, which is what a macvlan or veth interface delivers.
- **Only frames addressed to the interface's own MAC** are relayed. Frames the host sends itself are ignored, including the proxy's own replies, so a client that uses the relayed port as its source port can't create a loop.
- **The kernel still receives the client's packet too.** If the pod forwards IPv6 itself, a Thread node may also get a copy with the client's original source address. Its reply then takes the mesh's normal route.

## Development

```bash
gofmt -s -w .
go test -race ./...
golangci-lint run
docker build -t thread-udp-proxy .
```

See [AGENTS.md](AGENTS.md) for the quality gates and release pipeline.

## License

[MIT](LICENSE)
