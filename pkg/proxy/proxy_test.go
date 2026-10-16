package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/hononeko/thread-udp-proxy/pkg/packet"
)

var (
	proxyMAC  = net.HardwareAddr{0x02, 0, 0, 0, 0x10, 0}
	clientMAC = net.HardwareAddr{0x02, 0, 0, 0, 0x20, 0}
	clientIP  = netip.MustParseAddr("fd00:2:244::ff:fe00:2000")
)

// fakeConn feeds frames from in and records written frames on out.
type fakeConn struct {
	in      chan []byte
	out     chan []byte
	readErr chan error
}

func newFakeConn() *fakeConn {
	return &fakeConn{in: make(chan []byte, 16), out: make(chan []byte, 16), readErr: make(chan error, 1)}
}

func (c *fakeConn) ReadFrame(buf []byte) (int, error) {
	select {
	case frame := <-c.in:
		return copy(buf, frame), nil
	case err := <-c.readErr:
		return 0, err
	case <-time.After(10 * time.Millisecond):
		return 0, ErrTimeout
	}
}

func (c *fakeConn) WriteFrame(frame []byte) error {
	c.out <- frame
	return nil
}

func (c *fakeConn) HardwareAddr() net.HardwareAddr { return proxyMAC }
func (c *fakeConn) Close() error                   { return nil }

// startNode runs a stand-in Thread node on loopback that answers every datagram with "pong:" + payload.
func startNode(t *testing.T) netip.AddrPort {
	t.Helper()
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDPAddrPort(append([]byte("pong:"), buf[:n]...), from)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

func clientFrame(t *testing.T, dst netip.AddrPort, srcPort uint16, payload string) []byte {
	t.Helper()
	frame, err := packet.Build(packet.Datagram{
		DstMAC: proxyMAC, SrcMAC: clientMAC,
		SrcIP: clientIP, DstIP: dst.Addr(),
		SrcPort: srcPort, DstPort: dst.Port(),
		Payload: []byte(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func runProxy(t *testing.T, p *Proxy, conn FrameConn) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		done <- p.Run(ctx, conn)
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return after cancellation")
		}
	})
	return done
}

func awaitFrame(t *testing.T, conn *fakeConn) packet.Datagram {
	t.Helper()
	select {
	case frame := <-conn.out:
		d, err := packet.Parse(frame)
		if err != nil {
			t.Fatalf("proxy wrote an invalid frame: %v", err)
		}
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("no reply frame within 2s")
		return packet.Datagram{}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRelaysDatagramAndReply(t *testing.T) {
	node := startNode(t)
	conn := newFakeConn()
	p := New(Config{Port: node.Port(), Prefixes: []netip.Prefix{netip.MustParsePrefix("::1/128")}, IdleTimeout: time.Minute})
	runProxy(t, p, conn)

	conn.in <- clientFrame(t, node, 42557, "ping")
	got := awaitFrame(t, conn)

	if string(got.Payload) != "pong:ping" {
		t.Errorf("payload = %q, want %q", got.Payload, "pong:ping")
	}
	if got.SrcIP != node.Addr() || got.SrcPort != node.Port() {
		t.Errorf("reply source = %s, want the node %s", netip.AddrPortFrom(got.SrcIP, got.SrcPort), node)
	}
	if got.DstIP != clientIP || got.DstPort != 42557 {
		t.Errorf("reply destination = %s, want the client", netip.AddrPortFrom(got.DstIP, got.DstPort))
	}
	if got.DstMAC.String() != clientMAC.String() || got.SrcMAC.String() != proxyMAC.String() {
		t.Errorf("reply MACs = %s -> %s, want %s -> %s", got.SrcMAC, got.DstMAC, proxyMAC, clientMAC)
	}

	stats := p.Stats()
	if stats.Forwarded != 1 || stats.Replies != 1 || stats.Dropped != 0 || stats.ActiveMappings != 1 {
		t.Errorf("stats = %+v, want 1 forwarded, 1 reply, 0 dropped, 1 mapping", stats)
	}
}

func TestReusesMappingPerClient(t *testing.T) {
	node := startNode(t)
	conn := newFakeConn()
	p := New(Config{Port: node.Port(), Prefixes: []netip.Prefix{netip.MustParsePrefix("::1/128")}, IdleTimeout: time.Minute})
	runProxy(t, p, conn)

	for _, srcPort := range []uint16{1000, 1000, 2000} {
		conn.in <- clientFrame(t, node, srcPort, "x")
		if got := awaitFrame(t, conn); got.DstPort != srcPort {
			t.Fatalf("reply went to port %d, want %d", got.DstPort, srcPort)
		}
	}
	if got := p.Stats().ActiveMappings; got != 2 {
		t.Fatalf("ActiveMappings = %d, want 2 (one per client port)", got)
	}
}

func TestIgnoresOtherTraffic(t *testing.T) {
	node := startNode(t)
	conn := newFakeConn()
	p := New(Config{Port: node.Port(), Prefixes: []netip.Prefix{netip.MustParsePrefix("::1/128")}, IdleTimeout: time.Minute})
	runProxy(t, p, conn)

	conn.in <- []byte("not a frame")
	conn.in <- clientFrame(t, netip.AddrPortFrom(node.Addr(), node.Port()+1), 1000, "wrong port")
	conn.in <- clientFrame(t, netip.AddrPortFrom(netip.MustParseAddr("fd00::1"), node.Port()), 1000, "outside the prefixes")
	// Frames are handled in order, so once this one is answered the others have been seen.
	conn.in <- clientFrame(t, node, 1000, "relayed")

	if got := awaitFrame(t, conn); string(got.Payload) != "pong:relayed" {
		t.Fatalf("first reply = %q, want the relayed datagram's", got.Payload)
	}
	if stats := p.Stats(); stats.Forwarded != 1 || stats.Dropped != 0 {
		t.Fatalf("stats = %+v, want only the matching datagram forwarded and nothing dropped", stats)
	}
}

func TestExpiresIdleClients(t *testing.T) {
	node := startNode(t)
	conn := newFakeConn()
	p := New(Config{Port: node.Port(), Prefixes: []netip.Prefix{netip.MustParsePrefix("::1/128")}, IdleTimeout: 50 * time.Millisecond})
	runProxy(t, p, conn)

	conn.in <- clientFrame(t, node, 1000, "x")
	awaitFrame(t, conn)
	eventually(t, "the idle client to expire", func() bool { return p.Stats().ActiveMappings == 0 })
}

func TestCountsOpenSocketFailures(t *testing.T) {
	conn := newFakeConn()
	p := New(Config{Port: 5540, Prefixes: []netip.Prefix{netip.MustParsePrefix("fc00::/7")}, IdleTimeout: time.Minute})
	p.listen = func() (*net.UDPConn, error) { return nil, errors.New("no sockets left") }
	runProxy(t, p, conn)

	conn.in <- clientFrame(t, netip.MustParseAddrPort("[fd2f:741e:f59b:1::1]:5540"), 1000, "x")
	eventually(t, "the drop to be counted", func() bool { return p.Stats().Dropped == 1 })
	if got := p.Stats().ActiveMappings; got != 0 {
		t.Fatalf("ActiveMappings = %d, want 0", got)
	}
}

func TestRunReturnsReadErrors(t *testing.T) {
	conn := newFakeConn()
	p := New(Config{Port: 5540, Prefixes: []netip.Prefix{netip.MustParsePrefix("fc00::/7")}, IdleTimeout: time.Minute})
	done := runProxy(t, p, conn)

	conn.readErr <- errors.New("interface gone")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil, want the read error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after a read error")
	}
}

func TestSweepInterval(t *testing.T) {
	tests := []struct{ idle, want time.Duration }{
		{5 * time.Minute, 30 * time.Second},
		{20 * time.Second, 10 * time.Second},
		{time.Nanosecond, time.Millisecond},
	}
	for _, tt := range tests {
		if got := sweepInterval(tt.idle); got != tt.want {
			t.Errorf("sweepInterval(%s) = %s, want %s", tt.idle, got, tt.want)
		}
	}
}
