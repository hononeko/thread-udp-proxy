// Package proxy relays UDP datagrams that arrive on an infrastructure interface into a Thread mesh, and the replies
// back, by NATing each client through a local UDP socket.
//
// A client (e.g. matter-server) routes traffic for the Thread prefix to this host's MAC. The proxy picks those frames
// up from a raw packet socket, sends each payload to its Thread destination from a socket bound to this host, and
// writes every reply back to the client as a raw frame that appears to come straight from the Thread node. Thread
// nodes therefore only ever see this host as the source, so their replies route back through it.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hononeko/thread-udp-proxy/pkg/logger"
	"github.com/hononeko/thread-udp-proxy/pkg/packet"
)

// ErrTimeout is returned by FrameConn.ReadFrame when no frame arrived before the read deadline.
var ErrTimeout = errors.New("read timed out")

// FrameConn reads and writes raw Ethernet frames on one interface. One goroutine reads, while WriteFrame and
// HardwareAddr may be called from several goroutines at once, concurrently with ReadFrame.
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

// mapping is one client's NAT entry. The goroutine running Run owns the mappings map and conn's lifecycle, and the
// client's reply goroutine reads from conn; clientMAC and lastActive are shared between the two, hence atomic.
type mapping struct {
	conn       *net.UDPConn
	clientMAC  atomic.Pointer[net.HardwareAddr]
	lastActive atomic.Int64 // monotonic, see sinceStart
}

// start anchors sinceStart.
var start = time.Now()

// sinceStart is t on the monotonic clock as an integer, so idle checks are immune to wall-clock steps (NTP).
func sinceStart(t time.Time) int64 { return int64(t.Sub(start)) }

func (m *mapping) touch(now time.Time) { m.lastActive.Store(sinceStart(now)) }

func (m *mapping) setClientMAC(mac net.HardwareAddr) { m.clientMAC.Store(&mac) }

// Run relays traffic on conn until ctx is cancelled (returning nil, within one read timeout) or reading from conn
// fails. Each datagram is relayed by the goroutine that read it: frames by the one calling Run, replies by one
// goroutine per client, so nothing waits on a hand-off. Run closes every client socket and waits for their reply
// goroutines to finish before returning, so conn is no longer used once it returns; closing it is left to the caller.
func (p *Proxy) Run(ctx context.Context, conn FrameConn) error {
	// Owned by this goroutine alone.
	mappings := make(map[clientKey]*mapping)
	var replyWriters sync.WaitGroup
	defer func() {
		for _, m := range mappings {
			_ = m.conn.Close()
		}
		p.mappings.Add(-int64(len(mappings)))
		replyWriters.Wait()
	}()

	interval := sweepInterval(p.cfg.IdleTimeout)
	nextSweep := time.Now().Add(interval)
	buf := make([]byte, 65536)
	for ctx.Err() == nil {
		n, rx, err := conn.ReadFrame(buf)
		// Reads time out regularly, so idle clients are swept here too, without another goroutine touching mappings.
		if now := time.Now(); !now.Before(nextSweep) {
			p.expire(now, mappings)
			nextSweep = now.Add(interval)
		}
		if errors.Is(err, ErrTimeout) {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to read frames: %w", err)
		}
		p.handleFrame(buf[:n], rx, conn, mappings, &replyWriters)
	}
	return nil
}

// handleFrame relays one frame read from the interface. frame is only used until it returns.
func (p *Proxy) handleFrame(frame []byte, rx time.Time, conn FrameConn, mappings map[clientKey]*mapping, replyWriters *sync.WaitGroup) {
	d, err := packet.Parse(frame)
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
		m.setClientMAC(d.SrcMAC)
		mappings[key] = m
		p.mappings.Add(1)
		replyWriters.Go(func() { p.relayReplies(key, m, conn) })
		logger.Info("New client", "client", netip.AddrPortFrom(d.SrcIP, d.SrcPort), "via", udpConn.LocalAddr())
	} else if !bytes.Equal(*m.clientMAC.Load(), d.SrcMAC) {
		m.setClientMAC(d.SrcMAC)
	}
	m.touch(time.Now())

	dst := netip.AddrPortFrom(d.DstIP, d.DstPort)
	if _, err := m.conn.WriteToUDPAddrPort(d.Payload, dst); err != nil {
		p.dropped.Add(1)
		logger.Debug("Failed to forward datagram", "client", netip.AddrPortFrom(d.SrcIP, d.SrcPort), "dst", dst, "err", err)
		return
	}
	p.forwardLatency.observe(time.Since(rx))
	p.forwarded.Add(1)
	logger.Debug("Forwarded datagram", "client", netip.AddrPortFrom(d.SrcIP, d.SrcPort), "dst", dst, "bytes", len(d.Payload))
}

// relayReplies writes each reply arriving on m's socket back to the client as a frame on conn, until the socket is
// closed: when the client expires or Run stops, which is how cancellation reaches it.
func (p *Proxy) relayReplies(key clientKey, m *mapping, conn FrameConn) {
	buf := make([]byte, 65536)
	oob := make([]byte, oobSize)
	client := netip.AddrPortFrom(key.addr, key.port)
	for {
		n, oobn, _, from, err := m.conn.ReadMsgUDPAddrPort(buf, oob)
		if err != nil {
			return // The socket was closed: the mapping expired or the proxy stopped.
		}
		rx, ok := kernelTimestamp(oob[:oobn])
		if !ok {
			rx = time.Now()
		}
		m.touch(time.Now())

		frame, err := packet.Build(packet.Datagram{
			DstMAC:  *m.clientMAC.Load(),
			SrcMAC:  conn.HardwareAddr(),
			SrcIP:   from.Addr(),
			DstIP:   key.addr,
			SrcPort: from.Port(),
			DstPort: key.port,
			Payload: buf[:n],
		})
		if err == nil {
			err = conn.WriteFrame(frame)
		}
		if err != nil {
			p.dropped.Add(1)
			logger.Debug("Failed to return reply", "from", from, "client", client, "err", err)
			continue
		}
		p.replyLatency.observe(time.Since(rx))
		p.replies.Add(1)
		logger.Debug("Returned reply", "from", from, "client", client, "bytes", n)
	}
}

func (p *Proxy) expire(now time.Time, mappings map[clientKey]*mapping) {
	for key, m := range mappings {
		if time.Duration(sinceStart(now)-m.lastActive.Load()) > p.cfg.IdleTimeout {
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
