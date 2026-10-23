// Package proxy relays UDP datagrams that arrive on an infrastructure interface into a Thread mesh, and the replies
// back, by NATing each client through a local UDP socket.
//
// A client (e.g. matter-server) routes traffic for the Thread prefix to this host's MAC. The proxy picks those frames
// up from a raw packet socket, sends each payload to its Thread destination from a socket bound to this host's Thread
// interface, and writes every reply back to the client as a raw frame that appears to come straight from the Thread
// node. Thread nodes therefore only ever see this host as the source, so their replies route back through it.
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
	"syscall"
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
	// ThreadInterface is the interface into the Thread mesh (wpan0). Each client's socket is bound to it, so the proxy
	// only ever sends into the mesh, and only takes replies that come from it.
	ThreadInterface string
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
	cfg Config
	// threadIndex returns the Thread interface's index. otbr-agent recreates the interface when it restarts, with a new
	// index, and a socket bound to the old one can't send any more.
	threadIndex func() (int, error)
	// relays reports whether to relay a datagram for a destination: relayable, except in tests, which also relay to
	// loopback, where their stand-in Thread node runs.
	relays func(netip.Addr) bool

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
		cfg:    cfg,
		relays: relayable,
		threadIndex: func() (int, error) {
			ifi, err := net.InterfaceByName(cfg.ThreadInterface)
			if err != nil {
				return 0, fmt.Errorf("failed to find the Thread interface %q: %w", cfg.ThreadInterface, err)
			}
			return ifi.Index, nil
		},
	}
}

// listen opens a client's socket, bound to the Thread interface, and returns the index of the interface it's bound to.
func (p *Proxy) listen() (*net.UDPConn, int, error) {
	ifindex, err := p.threadIndex()
	if err != nil {
		return nil, 0, err
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error { return bindToInterface(c, ifindex) }}
	pc, err := lc.ListenPacket(context.Background(), "udp6", "[::]:0")
	if err != nil {
		return nil, 0, err
	}
	conn := pc.(*net.UDPConn)
	if err := enableTimestamps(conn); err != nil {
		logger.Debug("Kernel receive timestamps unavailable, measuring reply latency from the read", "err", err)
	}
	return conn, ifindex, nil
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
	conn *net.UDPConn
	// threadIndex is the index of the Thread interface conn is bound to.
	threadIndex int
	clientMAC   atomic.Pointer[net.HardwareAddr]
	lastActive  atomic.Int64 // monotonic, see sinceStart
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
		// Reads time out regularly, so clients are swept here too, without another goroutine touching mappings.
		if now := time.Now(); !now.Before(nextSweep) {
			p.sweep(now, mappings)
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
		udpConn, threadIndex, err := p.listen()
		if err != nil {
			p.dropped.Add(1)
			logger.Warn("Failed to open a socket for a new client", "client", netip.AddrPortFrom(d.SrcIP, d.SrcPort), "err", err)
			return
		}
		m = &mapping{conn: udpConn, threadIndex: threadIndex}
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
	if debugging() {
		logger.Debug("Forwarded datagram", "client", netip.AddrPortFrom(d.SrcIP, d.SrcPort), "dst", dst, "bytes", len(d.Payload))
	}
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
		if debugging() {
			logger.Debug("Returned reply", "from", from, "client", client, "bytes", n)
		}
	}
}

// sweep closes the sockets of clients that have been idle for longer than the idle timeout, and of those bound to a
// Thread interface that was recreated since, which can't send any more. A client's next datagram opens a new socket.
func (p *Proxy) sweep(now time.Time, mappings map[clientKey]*mapping) {
	if len(mappings) == 0 {
		return
	}
	// While the interface is missing (otbr-agent restarting), sockets are left alone: whether they're stale is only
	// known once it's back.
	threadIndex, err := p.threadIndex()
	for key, m := range mappings {
		idle := time.Duration(sinceStart(now)-m.lastActive.Load()) > p.cfg.IdleTimeout
		stale := err == nil && m.threadIndex != threadIndex
		if !idle && !stale {
			continue
		}
		_ = m.conn.Close()
		delete(mappings, key)
		p.mappings.Add(-1)
		client := netip.AddrPortFrom(key.addr, key.port)
		if idle {
			logger.Info("Expired idle client", "client", client)
		} else {
			logger.Info("Closed a client's socket bound to the old Thread interface", "client", client)
		}
	}
}

// relayable reports whether the proxy relays datagrams for addr: any unicast address beyond this link, i.e. not
// link-local, loopback, multicast or unspecified, nor IPv4-mapped. A client's socket would send those through the
// Thread interface it's bound to, into the mesh. Every other destination is either in the mesh or unreachable from
// that socket, so there's no prefix to match, and the proxy keeps working when the Thread network's prefixes change.
func relayable(addr netip.Addr) bool {
	return addr.IsGlobalUnicast() && !addr.Is4In6()
}

// debugging reports whether debug logs are written. Each relayed datagram's debug log is only built when they are:
// building one costs more than the rest of the proxy's own work on the datagram, even when the logger then drops it,
// because the logger looks up its caller before checking the level, and boxing the arguments allocates.
func debugging() bool { return logger.GetLevel() == "debug" }

// sweepInterval sweeps often enough to expire idle clients within half a timeout of going idle, and to notice a
// recreated Thread interface within 5s.
func sweepInterval(idle time.Duration) time.Duration {
	return min(max(idle/2, time.Millisecond), 5*time.Second)
}
