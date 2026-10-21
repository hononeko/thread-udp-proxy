// Package proxy relays UDP datagrams that arrive on an infrastructure interface into a Thread mesh, and the replies
// back, by NATing each client through a local UDP socket.
//
// A client (e.g. matter-server) routes traffic for the Thread prefix to this host's MAC. The proxy picks those frames
// up from a raw packet socket, sends each payload to its Thread destination from a socket bound to this host, and
// writes every reply back to the client as a raw frame that appears to come straight from the Thread node. Thread
// nodes therefore only ever see this host as the source, so their replies route back through it.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/hononeko/thread-udp-proxy/pkg/logger"
	"github.com/hononeko/thread-udp-proxy/pkg/packet"
)

// ErrTimeout is returned by FrameConn.ReadFrame when no frame arrived before the read deadline.
var ErrTimeout = errors.New("read timed out")

// FrameConn reads and writes raw Ethernet frames on one interface.
type FrameConn interface {
	// ReadFrame reads one inbound frame addressed to this host into buf, and returns when the kernel received it (or
	// the read time if that's unknown). It returns ErrTimeout when none arrived in time, so the caller can check for
	// cancellation.
	ReadFrame(buf []byte) (int, time.Time, error)
	// WriteFrame sends one complete Ethernet frame.
	WriteFrame(frame []byte) error
	// HardwareAddr is the interface's MAC address, used as the source of reply frames.
	HardwareAddr() net.HardwareAddr
	Close() error
}

// Config selects the traffic the proxy relays.
type Config struct {
	// Port is the UDP destination port to relay (5540 for Matter).
	Port uint16
	// Prefixes are the destinations to relay, i.e. the Thread mesh's prefixes.
	Prefixes []netip.Prefix
	// IdleTimeout closes a client's socket after this long without traffic in either direction.
	IdleTimeout time.Duration
}

// Stats are cumulative counters over the proxy's lifetime.
type Stats struct {
	Forwarded uint64
	Replies   uint64
	Dropped   uint64
	// Ignored counts frames read from the interface that weren't ours to relay.
	Ignored        uint64
	ActiveMappings int64
	// ForwardLatency is the time from a client frame reaching the interface to its datagram being sent into the mesh.
	ForwardLatency Histogram
	// ReplyLatency is the time from a reply reaching its socket to its frame being written back to the client.
	ReplyLatency Histogram
}

// Proxy relays matching datagrams between clients and the Thread mesh. Run may be called again with a new FrameConn
// after it returns; Stats accumulate across runs.
type Proxy struct {
	cfg    Config
	listen func() (*net.UDPConn, error)

	forwarded atomic.Uint64
	replies   atomic.Uint64
	dropped   atomic.Uint64
	ignored   atomic.Uint64
	mappings  atomic.Int64

	forwardLatency histogram
	replyLatency   histogram
}

// New returns a proxy for cfg.
func New(cfg Config) *Proxy {
	return &Proxy{
		cfg: cfg,
		listen: func() (*net.UDPConn, error) {
			conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified})
			if err != nil {
				return nil, err
			}
			if err := enableTimestamps(conn); err != nil {
				logger.Debug("Kernel receive timestamps unavailable, measuring reply latency from the read", "err", err)
			}
			return conn, nil
		},
	}
}

// Stats returns a snapshot of the counters.
func (p *Proxy) Stats() Stats {
	return Stats{
		Forwarded:      p.forwarded.Load(),
		Replies:        p.replies.Load(),
		Dropped:        p.dropped.Load(),
		Ignored:        p.ignored.Load(),
		ActiveMappings: p.mappings.Load(),
		ForwardLatency: p.forwardLatency.snapshot(),
		ReplyLatency:   p.replyLatency.snapshot(),
	}
}

type clientKey struct {
	addr netip.Addr
	port uint16
}

type mapping struct {
	clientMAC  net.HardwareAddr
	conn       *net.UDPConn
	lastActive time.Time
}

type frame struct {
	data []byte
	rx   time.Time
}

type reply struct {
	key     clientKey
	from    netip.AddrPort
	payload []byte
	rx      time.Time
}

// Run relays traffic on conn until ctx is cancelled (returning nil) or reading from conn fails. It closes every
// client socket before returning; closing conn is left to the caller.
func (p *Proxy) Run(ctx context.Context, conn FrameConn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	frames := make(chan frame, 1024)
	replies := make(chan reply, 1024)
	readErr := make(chan error, 1)
	go p.readFrames(ctx, conn, frames, readErr)

	// Owned by this goroutine alone.
	mappings := make(map[clientKey]*mapping)
	defer func() {
		for _, m := range mappings {
			_ = m.conn.Close()
		}
		p.mappings.Add(-int64(len(mappings)))
	}()

	sweep := time.NewTicker(sweepInterval(p.cfg.IdleTimeout))
	defer sweep.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return fmt.Errorf("failed to read frames: %w", err)
		case f := <-frames:
			p.handleFrame(ctx, f, mappings, replies)
		case r := <-replies:
			p.handleReply(conn, r, mappings)
		case now := <-sweep.C:
			p.expire(now, mappings)
		}
	}
}

func (p *Proxy) readFrames(ctx context.Context, conn FrameConn, frames chan<- frame, readErr chan<- error) {
	buf := make([]byte, 65536)
	for ctx.Err() == nil {
		n, rx, err := conn.ReadFrame(buf)
		if errors.Is(err, ErrTimeout) {
			continue
		}
		if err != nil {
			readErr <- err
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		select {
		case frames <- frame{data: data, rx: rx}:
		case <-ctx.Done():
			return
		}
	}
}

func (p *Proxy) handleFrame(ctx context.Context, f frame, mappings map[clientKey]*mapping, replies chan<- reply) {
	d, err := packet.Parse(f.data)
	if err != nil || d.DstPort != p.cfg.Port || !p.relays(d.DstIP) {
		// Other traffic for this host (neighbor discovery, the host's own sockets, ...): not ours to relay.
		p.ignored.Add(1)
		return
	}

	key := clientKey{addr: d.SrcIP, port: d.SrcPort}
	m, ok := mappings[key]
	if !ok {
		udpConn, err := p.listen()
		if err != nil {
			p.dropped.Add(1)
			logger.Warn("Failed to open a socket for a new client", "client", netip.AddrPortFrom(d.SrcIP, d.SrcPort), "err", err)
			return
		}
		m = &mapping{conn: udpConn}
		mappings[key] = m
		p.mappings.Add(1)
		go readReplies(ctx, key, udpConn, replies)
		logger.Info("New client", "client", netip.AddrPortFrom(d.SrcIP, d.SrcPort), "via", udpConn.LocalAddr())
	}
	m.clientMAC = d.SrcMAC
	m.lastActive = time.Now()

	dst := netip.AddrPortFrom(d.DstIP, d.DstPort)
	if _, err := m.conn.WriteToUDPAddrPort(d.Payload, dst); err != nil {
		p.dropped.Add(1)
		logger.Debug("Failed to forward datagram", "client", netip.AddrPortFrom(d.SrcIP, d.SrcPort), "dst", dst, "err", err)
		return
	}
	p.forwardLatency.observe(time.Since(f.rx))
	p.forwarded.Add(1)
	logger.Debug("Forwarded datagram", "client", netip.AddrPortFrom(d.SrcIP, d.SrcPort), "dst", dst, "bytes", len(d.Payload))
}

func readReplies(ctx context.Context, key clientKey, conn *net.UDPConn, replies chan<- reply) {
	buf := make([]byte, 65536)
	oob := make([]byte, oobSize)
	for {
		n, oobn, _, from, err := conn.ReadMsgUDPAddrPort(buf, oob)
		if err != nil {
			return // The socket was closed: the mapping expired or the proxy stopped.
		}
		rx, ok := kernelTimestamp(oob[:oobn])
		if !ok {
			rx = time.Now()
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		select {
		case replies <- reply{key: key, from: from, payload: payload, rx: rx}:
		case <-ctx.Done():
			return
		}
	}
}

func (p *Proxy) handleReply(conn FrameConn, r reply, mappings map[clientKey]*mapping) {
	m, ok := mappings[r.key]
	if !ok {
		return // Expired while the reply was queued.
	}
	m.lastActive = time.Now()

	client := netip.AddrPortFrom(r.key.addr, r.key.port)
	frame, err := packet.Build(packet.Datagram{
		DstMAC:  m.clientMAC,
		SrcMAC:  conn.HardwareAddr(),
		SrcIP:   r.from.Addr(),
		DstIP:   r.key.addr,
		SrcPort: r.from.Port(),
		DstPort: r.key.port,
		Payload: r.payload,
	})
	if err == nil {
		err = conn.WriteFrame(frame)
	}
	if err != nil {
		p.dropped.Add(1)
		logger.Debug("Failed to return reply", "from", r.from, "client", client, "err", err)
		return
	}
	p.replyLatency.observe(time.Since(r.rx))
	p.replies.Add(1)
	logger.Debug("Returned reply", "from", r.from, "client", client, "bytes", len(r.payload))
}

func (p *Proxy) expire(now time.Time, mappings map[clientKey]*mapping) {
	for key, m := range mappings {
		if now.Sub(m.lastActive) > p.cfg.IdleTimeout {
			_ = m.conn.Close()
			delete(mappings, key)
			p.mappings.Add(-1)
			logger.Info("Expired idle client", "client", netip.AddrPortFrom(key.addr, key.port))
		}
	}
}

func (p *Proxy) relays(addr netip.Addr) bool {
	for _, prefix := range p.cfg.Prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// sweepInterval checks for idle clients often enough to expire them within half a timeout of going idle.
func sweepInterval(idle time.Duration) time.Duration {
	return min(max(idle/2, time.Millisecond), 30*time.Second)
}
