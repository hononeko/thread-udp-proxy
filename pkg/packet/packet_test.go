package packet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func sample() Datagram {
	return Datagram{
		DstMAC:  net.HardwareAddr{0x02, 0, 0, 0, 0x10, 0},
		SrcMAC:  net.HardwareAddr{0x02, 0, 0, 0, 0x20, 0},
		SrcIP:   netip.MustParseAddr("fd00:2:244::ff:fe00:2000"),
		DstIP:   netip.MustParseAddr("fd2f:741e:f59b:1:6f60:c19b:2907:cd54"),
		SrcPort: 42557,
		DstPort: 5540,
		Payload: []byte("matter"),
	}
}

func TestBuildParseRoundTrip(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte("x"), []byte("even"), bytes.Repeat([]byte{0xab}, 1200)} {
		want := sample()
		want.Payload = payload

		frame, err := Build(want)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(frame) != MinFrameLen+len(payload) {
			t.Fatalf("frame length = %d, want %d", len(frame), MinFrameLen+len(payload))
		}

		got, err := Parse(frame)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if !bytes.Equal(got.DstMAC, want.DstMAC) || !bytes.Equal(got.SrcMAC, want.SrcMAC) {
			t.Errorf("MACs = %s -> %s, want %s -> %s", got.SrcMAC, got.DstMAC, want.SrcMAC, want.DstMAC)
		}
		if got.SrcIP != want.SrcIP || got.DstIP != want.DstIP {
			t.Errorf("IPs = %s -> %s, want %s -> %s", got.SrcIP, got.DstIP, want.SrcIP, want.DstIP)
		}
		if got.SrcPort != want.SrcPort || got.DstPort != want.DstPort {
			t.Errorf("ports = %d -> %d, want %d -> %d", got.SrcPort, got.DstPort, want.SrcPort, want.DstPort)
		}
		if !bytes.Equal(got.Payload, payload) {
			t.Errorf("payload = %q, want %q", got.Payload, payload)
		}

		// The checksum must verify: summing the segment including its checksum gives zero.
		if sum := Checksum(want.SrcIP.As16(), want.DstIP.As16(), frame[ethHeaderLen+ipv6HeaderLen:]); sum != 0 {
			t.Errorf("checksum does not verify (residual %#04x) for %d-byte payload", sum, len(payload))
		}
	}
}

func TestChecksumKnownVector(t *testing.T) {
	// ::1 -> ::1, port 1 -> 2, payload "abc". By hand (RFC 1071): pseudo-header 0x0001 + 0x0001 + length 0x000b +
	// next header 0x0011, segment 0x0001 + 0x0002 + 0x000b + 0x0000 + 0x6162 + 0x6300 = 0xc48e, complemented 0x3b71.
	src := netip.MustParseAddr("::1").As16()
	segment := []byte{0, 1, 0, 2, 0, 11, 0, 0, 'a', 'b', 'c'}
	if got, want := Checksum(src, src, segment), uint16(0x3b71); got != want {
		t.Fatalf("Checksum = %#04x, want %#04x", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	valid, err := Build(sample())
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(f func([]byte) []byte) []byte {
		return f(bytes.Clone(valid))
	}

	tests := []struct {
		name  string
		frame []byte
		want  error
	}{
		{"too short", valid[:MinFrameLen-1], ErrTooShort},
		{"IPv4 ethertype", mutate(func(b []byte) []byte { binary.BigEndian.PutUint16(b[12:14], 0x0800); return b }), ErrNotIPv6},
		{"IP version 4", mutate(func(b []byte) []byte { b[14] = 0x45; return b }), ErrNotIPv6},
		{"TCP", mutate(func(b []byte) []byte { b[20] = 6; return b }), ErrNotUDP},
		{"IPv6 payload longer than frame", mutate(func(b []byte) []byte { binary.BigEndian.PutUint16(b[18:20], 9000); return b }), ErrBadLength},
		{"UDP length below header", mutate(func(b []byte) []byte { binary.BigEndian.PutUint16(b[58:60], 4); return b }), ErrBadLength},
		{"UDP length beyond IPv6 payload", mutate(func(b []byte) []byte { binary.BigEndian.PutUint16(b[58:60], 9000); return b }), ErrBadLength},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse(tt.frame); !errors.Is(err, tt.want) {
				t.Fatalf("Parse error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestParseIgnoresEthernetPadding(t *testing.T) {
	frame, err := Build(sample())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(append(frame, 0, 0, 0, 0))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if string(got.Payload) != "matter" {
		t.Fatalf("payload = %q, want %q", got.Payload, "matter")
	}
}

func TestBuildRejectsInvalidDatagrams(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Datagram)
	}{
		{"IPv4 source", func(d *Datagram) { d.SrcIP = netip.MustParseAddr("192.168.4.1") }},
		{"missing destination", func(d *Datagram) { d.DstIP = netip.Addr{} }},
		{"short MAC", func(d *Datagram) { d.SrcMAC = net.HardwareAddr{1, 2, 3} }},
		{"oversized payload", func(d *Datagram) { d.Payload = make([]byte, MaxPayloadLen+1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := sample()
			tt.modify(&d)
			if _, err := Build(d); err == nil {
				t.Fatal("Build succeeded, want an error")
			}
		})
	}
}
