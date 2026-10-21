//go:build linux

package proxy

import (
	"net"
	"testing"
	"time"
)

func TestKernelTimestamps(t *testing.T) {
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := enableTimestamps(conn); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	if _, err := conn.WriteToUDPAddrPort([]byte("x"), conn.LocalAddr().(*net.UDPAddr).AddrPort()); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	oob := make([]byte, oobSize)
	_, oobn, _, _, err := conn.ReadMsgUDPAddrPort(make([]byte, 16), oob)
	if err != nil {
		t.Fatal(err)
	}

	rx, ok := kernelTimestamp(oob[:oobn])
	if !ok {
		t.Fatalf("no kernel timestamp in %d bytes of control messages", oobn)
	}
	// Allow for the coarser wall clock the kernel stamps with.
	if rx.Before(before.Add(-time.Second)) || rx.After(time.Now().Add(time.Second)) {
		t.Fatalf("kernel timestamp %s is not around the send at %s", rx, before)
	}
}

func TestKernelTimestampIgnoresOtherMessages(t *testing.T) {
	if _, ok := kernelTimestamp(nil); ok {
		t.Error("found a timestamp without control messages")
	}
	if _, ok := kernelTimestamp([]byte{1, 2, 3}); ok {
		t.Error("found a timestamp in a truncated control message")
	}
}
