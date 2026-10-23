package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
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

func (c *fakeConn) ReadFrame(buf []byte) (int, time.Time, error) {
	select {
	case frame := <-c.in:
		return copy(buf, frame), time.Now(), nil
	case err := <-c.readErr:
		return 0, time.Time{}, err
	case <-time.After(10 * time.Millisecond):
		return 0, time.Time{}, ErrTimeout
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

// newProxy returns a proxy that relays to loopback, where startNode runs: lo stands in for the Thread interface.
func newProxy(port uint16, idleTimeout time.Duration) *Proxy {
	p := New(Config{Port: port, ThreadInterface: "lo", IdleTimeout: idleTimeout})
	p.relays = func(addr netip.Addr) bool { return addr.IsLoopback() || relayable(addr) }
	return p
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
	p := newProxy(node.Port(), time.Minute)
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

	// The reply is counted after its frame is written, so wait for that before checking the counters.
	eventually(t, "the reply to be counted", func() bool { return p.Stats().Replies == 1 })
	stats := p.Stats()
	if stats.Forwarded != 1 || stats.Replies != 1 || stats.Dropped != 0 || stats.ActiveMappings != 1 {
		t.Errorf("stats = %+v, want 1 forwarded, 1 reply, 0 dropped, 1 mapping", stats)
	}
	if stats.ForwardLatency.Count != 1 || stats.ReplyLatency.Count != 1 {
		t.Errorf("latency counts = %d forward, %d reply, want 1 each", stats.ForwardLatency.Count, stats.ReplyLatency.Count)
	}
}

func TestReusesMappingPerClient(t *testing.T) {
	node := startNode(t)
	conn := newFakeConn()
	p := newProxy(node.Port(), time.Minute)
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

func TestRelaysClientsConcurrently(t *testing.T) {
	node := startNode(t)
	conn := newFakeConn()
	p := newProxy(node.Port(), time.Minute)
	runProxy(t, p, conn)

	// Every client's reply goroutine writes to conn on its own, so replies may arrive in any order.
	const clients = 8
	for i := range clients {
		conn.in <- clientFrame(t, node, uint16(3000+i), "x")
	}
	seen := make(map[uint16]bool)
	for range clients {
		seen[awaitFrame(t, conn).DstPort] = true
	}
	if len(seen) != clients {
		t.Fatalf("replies reached %d distinct clients, want %d", len(seen), clients)
	}
}

func TestRepliesFollowClientMAC(t *testing.T) {
	node := startNode(t)
	conn := newFakeConn()
	p := newProxy(node.Port(), time.Minute)
	runProxy(t, p, conn)

	conn.in <- clientFrame(t, node, 1000, "x")
	if got := awaitFrame(t, conn); got.DstMAC.String() != clientMAC.String() {
		t.Fatalf("reply went to %s, want %s", got.DstMAC, clientMAC)
	}

	// Same client address and port, new MAC (e.g. a failover): replies must follow it.
	moved := net.HardwareAddr{0x02, 0, 0, 0, 0x30, 0}
	frame := clientFrame(t, node, 1000, "y")
	copy(frame[6:12], moved)
	conn.in <- frame
	if got := awaitFrame(t, conn); got.DstMAC.String() != moved.String() {
		t.Fatalf("reply went to %s, want the new MAC %s", got.DstMAC, moved)
	}
	if got := p.Stats().ActiveMappings; got != 1 {
		t.Fatalf("ActiveMappings = %d, want 1", got)
	}
}

// gatedConn holds every WriteFrame until release is closed.
type gatedConn struct {
	*fakeConn
	writing  chan struct{} // closed when the first write starts
	release  chan struct{}
	once     sync.Once
	inFlight atomic.Int32
}

func (c *gatedConn) WriteFrame([]byte) error {
	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	c.once.Do(func() { close(c.writing) })
	<-c.release
	return nil
}

func TestRunWaitsForReplyWrites(t *testing.T) {
	node := startNode(t)
	conn := &gatedConn{fakeConn: newFakeConn(), writing: make(chan struct{}), release: make(chan struct{})}
	p := newProxy(node.Port(), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inFlightAtReturn := make(chan int32, 1)
	go func() {
		_ = p.Run(ctx, conn)
		inFlightAtReturn <- conn.inFlight.Load()
	}()

	conn.in <- clientFrame(t, node, 1000, "x")
	select {
	case <-conn.writing:
	case <-time.After(2 * time.Second):
		t.Fatal("no reply write started within 2s")
	}
	cancel()

	// Run sees the cancellation within one read timeout (10ms here), but the caller closes conn once Run returns, so
	// it must wait for the reply still being written.
	select {
	case <-inFlightAtReturn:
		t.Fatal("Run returned while a reply was still being written to conn")
	case <-time.After(100 * time.Millisecond):
	}
	close(conn.release)
	select {
	case n := <-inFlightAtReturn:
		if n != 0 {
			t.Fatalf("%d writes still in flight when Run returned", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the reply write finished")
	}
}

func TestIgnoresOtherTraffic(t *testing.T) {
	node := startNode(t)
	conn := newFakeConn()
	p := newProxy(node.Port(), time.Minute)
	runProxy(t, p, conn)

	conn.in <- []byte("not a frame")
	conn.in <- clientFrame(t, netip.AddrPortFrom(node.Addr(), node.Port()+1), 1000, "wrong port")
	conn.in <- clientFrame(t, netip.AddrPortFrom(netip.MustParseAddr("fe80::1"), node.Port()), 1000, "link-local")
	// Frames are handled in order, so once this one is answered the others have been seen.
	conn.in <- clientFrame(t, node, 1000, "relayed")

	if got := awaitFrame(t, conn); string(got.Payload) != "pong:relayed" {
		t.Fatalf("first reply = %q, want the relayed datagram's", got.Payload)
	}
	if stats := p.Stats(); stats.Forwarded != 1 || stats.Dropped != 0 || stats.Ignored != 3 {
		t.Fatalf("stats = %+v, want only the matching datagram forwarded, nothing dropped and 3 ignored", stats)
	}
}

func TestRelaysOnlyThroughTheThreadInterface(t *testing.T) {
	node := startNode(t)
	conn := newFakeConn()
	p := newProxy(node.Port(), time.Minute)
	runProxy(t, p, conn)

	// The client's socket is bound to lo, which has no route anywhere else, the way wpan0 has none to the cluster
	// network or the internet.
	conn.in <- clientFrame(t, netip.AddrPortFrom(netip.MustParseAddr("2001:db8::1"), node.Port()), 1000, "elsewhere")
	conn.in <- clientFrame(t, node, 1000, "relayed")

	if got := awaitFrame(t, conn); string(got.Payload) != "pong:relayed" {
		t.Fatalf("first reply = %q, want the relayed datagram's", got.Payload)
	}
	if stats := p.Stats(); stats.Forwarded != 1 || stats.Dropped != 1 {
		t.Fatalf("stats = %+v, want 1 forwarded and the datagram for elsewhere dropped", stats)
	}
}

func TestClosesSocketsBoundToARecreatedThreadInterface(t *testing.T) {
	node := startNode(t)
	conn := newFakeConn()
	p := newProxy(node.Port(), time.Minute)
	lo, err := p.threadIndex()
	if err != nil {
		t.Fatal(err)
	}
	// Driven directly instead of through Run, which only sweeps every few seconds.
	mappings := make(map[clientKey]*mapping)
	var replyWriters sync.WaitGroup
	t.Cleanup(func() {
		for _, m := range mappings {
			_ = m.conn.Close()
		}
		replyWriters.Wait()
	})
	p.handleFrame(clientFrame(t, node, 1000, "x"), time.Now(), conn, mappings, &replyWriters)
	awaitFrame(t, conn)

	steps := []struct {
		name        string
		threadIndex func() (int, error)
		want        int
	}{
		{"unchanged", func() (int, error) { return lo, nil }, 1},
		// otbr-agent restarting: whether the socket is stale is only known once the interface is back.
		{"missing", func() (int, error) { return 0, errors.New("no such network interface") }, 1},
		{"recreated with a new index", func() (int, error) { return lo + 1, nil }, 0},
	}
	for _, step := range steps {
		p.threadIndex = step.threadIndex
		p.sweep(time.Now(), mappings)
		if len(mappings) != step.want || p.Stats().ActiveMappings != int64(step.want) {
			t.Fatalf("Thread interface %s: %d mappings (%d active), want %d", step.name, len(mappings), p.Stats().ActiveMappings, step.want)
		}
	}
}

func TestRelayable(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"fd2f:741e:f59b:1::1", true}, // an OMR prefix
		{"fd71:3c82:737a:1::1", true}, // the one the border routers elected next
		{"2001:db8:1:2::1", true},     // a global OMR prefix, from DHCPv6 prefix delegation
		{"fe80::1", false},            // link-local: this host itself
		{"::1", false},
		{"ff05::1", false},
		{"::", false},
		{"::ffff:192.0.2.1", false},
	}
	for _, tt := range tests {
		if got := relayable(netip.MustParseAddr(tt.addr)); got != tt.want {
			t.Errorf("relayable(%s) = %t, want %t", tt.addr, got, tt.want)
		}
	}
}

func TestExpiresIdleClients(t *testing.T) {
	node := startNode(t)
	conn := newFakeConn()
	p := newProxy(node.Port(), 50*time.Millisecond)
	runProxy(t, p, conn)

	conn.in <- clientFrame(t, node, 1000, "x")
	awaitFrame(t, conn)
	eventually(t, "the idle client to expire", func() bool { return p.Stats().ActiveMappings == 0 })
}

func TestCountsOpenSocketFailures(t *testing.T) {
	conn := newFakeConn()
	p := New(Config{Port: 5540, ThreadInterface: "missing0", IdleTimeout: time.Minute})
	runProxy(t, p, conn)

	conn.in <- clientFrame(t, netip.MustParseAddrPort("[fd2f:741e:f59b:1::1]:5540"), 1000, "x")
	eventually(t, "the drop to be counted", func() bool { return p.Stats().Dropped == 1 })
	if got := p.Stats().ActiveMappings; got != 0 {
		t.Fatalf("ActiveMappings = %d, want 0", got)
	}
}

func TestRunReturnsReadErrors(t *testing.T) {
	conn := newFakeConn()
	p := New(Config{Port: 5540, IdleTimeout: time.Minute})
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
		{5 * time.Minute, 5 * time.Second},
		{4 * time.Second, 2 * time.Second},
		{time.Nanosecond, time.Millisecond},
	}
	for _, tt := range tests {
		if got := sweepInterval(tt.idle); got != tt.want {
			t.Errorf("sweepInterval(%s) = %s, want %s", tt.idle, got, tt.want)
		}
	}
}
