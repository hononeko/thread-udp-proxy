// Package packet parses and builds the Ethernet/IPv6/UDP frames the proxy relays.
package packet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"slices"
)

const (
	// EtherTypeIPv6 is the EtherType of IPv6 frames.
	EtherTypeIPv6 = 0x86DD

	ethHeaderLen  = 14
	ipv6HeaderLen = 40
	udpHeaderLen  = 8
	protoUDP      = 17
	hopLimit      = 64

	// MinFrameLen is the length of an Ethernet frame carrying an empty UDP datagram over IPv6.
	MinFrameLen = ethHeaderLen + ipv6HeaderLen + udpHeaderLen

	// MaxPayloadLen is the largest UDP payload a single IPv6 datagram can carry.
	MaxPayloadLen = math.MaxUint16 - udpHeaderLen
)

var (
	ErrTooShort  = errors.New("frame too short")
	ErrNotIPv6   = errors.New("not an IPv6 frame")
	ErrNotUDP    = errors.New("not a UDP datagram")
	ErrBadLength = errors.New("inconsistent length fields")
)

// Datagram is a UDP datagram with the addresses of the Ethernet frame and the IPv6 packet that carry it.
type Datagram struct {
	DstMAC  net.HardwareAddr
	SrcMAC  net.HardwareAddr
	SrcIP   netip.Addr
	DstIP   netip.Addr
	SrcPort uint16
	DstPort uint16
	Payload []byte
}

// Parse decodes an Ethernet frame carrying a UDP datagram directly over IPv6 (no extension headers).
// The returned Payload aliases frame; the MAC addresses are copies.
func Parse(frame []byte) (Datagram, error) {
	if len(frame) < MinFrameLen {
		return Datagram{}, ErrTooShort
	}
	if binary.BigEndian.Uint16(frame[12:14]) != EtherTypeIPv6 {
		return Datagram{}, ErrNotIPv6
	}

	ip := frame[ethHeaderLen : ethHeaderLen+ipv6HeaderLen]
	if ip[0]>>4 != 6 {
		return Datagram{}, ErrNotIPv6
	}
	if ip[6] != protoUDP {
		return Datagram{}, ErrNotUDP
	}

	udp := frame[ethHeaderLen+ipv6HeaderLen:]
	ipPayloadLen := int(binary.BigEndian.Uint16(ip[4:6]))
	if ipPayloadLen > len(udp) {
		return Datagram{}, ErrBadLength
	}
	udp = udp[:ipPayloadLen]

	udpLen := int(binary.BigEndian.Uint16(udp[4:6]))
	if udpLen < udpHeaderLen || udpLen > len(udp) {
		return Datagram{}, ErrBadLength
	}

	return Datagram{
		DstMAC:  net.HardwareAddr(slices.Clone(frame[0:6])),
		SrcMAC:  net.HardwareAddr(slices.Clone(frame[6:12])),
		SrcIP:   netip.AddrFrom16([16]byte(ip[8:24])),
		DstIP:   netip.AddrFrom16([16]byte(ip[24:40])),
		SrcPort: binary.BigEndian.Uint16(udp[0:2]),
		DstPort: binary.BigEndian.Uint16(udp[2:4]),
		Payload: udp[udpHeaderLen:udpLen],
	}, nil
}

// Build encodes d as an Ethernet frame carrying an IPv6 UDP datagram with a valid checksum.
func Build(d Datagram) ([]byte, error) {
	if !d.SrcIP.Is6() || !d.DstIP.Is6() {
		return nil, fmt.Errorf("addresses must be IPv6, got %s -> %s", d.SrcIP, d.DstIP)
	}
	if len(d.DstMAC) != 6 || len(d.SrcMAC) != 6 {
		return nil, fmt.Errorf("MAC addresses must be 6 bytes, got %d and %d", len(d.DstMAC), len(d.SrcMAC))
	}
	if len(d.Payload) > MaxPayloadLen {
		return nil, fmt.Errorf("payload of %d bytes exceeds %d", len(d.Payload), MaxPayloadLen)
	}
	udpLen := uint16(udpHeaderLen + len(d.Payload)) //nolint:gosec // bounded by MaxPayloadLen above

	frame := make([]byte, ethHeaderLen+ipv6HeaderLen+int(udpLen))
	copy(frame[0:6], d.DstMAC)
	copy(frame[6:12], d.SrcMAC)
	binary.BigEndian.PutUint16(frame[12:14], EtherTypeIPv6)

	src, dst := d.SrcIP.As16(), d.DstIP.As16()
	ip := frame[ethHeaderLen : ethHeaderLen+ipv6HeaderLen]
	binary.BigEndian.PutUint32(ip[0:4], 6<<28)
	binary.BigEndian.PutUint16(ip[4:6], udpLen)
	ip[6] = protoUDP
	ip[7] = hopLimit
	copy(ip[8:24], src[:])
	copy(ip[24:40], dst[:])

	udp := frame[ethHeaderLen+ipv6HeaderLen:]
	binary.BigEndian.PutUint16(udp[0:2], d.SrcPort)
	binary.BigEndian.PutUint16(udp[2:4], d.DstPort)
	binary.BigEndian.PutUint16(udp[4:6], udpLen)
	copy(udp[udpHeaderLen:], d.Payload)

	sum := Checksum(src, dst, udp)
	if sum == 0 {
		// Zero means "no checksum", which IPv6 forbids for UDP (RFC 8200 §8.1).
		sum = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:8], sum)
	return frame, nil
}

// Checksum returns the UDP checksum of segment over the IPv6 pseudo-header (RFC 8200 §8.1). Computed over a segment
// whose checksum field is already filled in, it returns 0 when that checksum is valid.
func Checksum(src, dst [16]byte, segment []byte) uint16 {
	var sum uint32
	add := func(b []byte) {
		for i := 0; i+1 < len(b); i += 2 {
			sum += uint32(b[i])<<8 | uint32(b[i+1])
		}
		if len(b)%2 == 1 {
			sum += uint32(b[len(b)-1]) << 8
		}
	}
	add(src[:])
	add(dst[:])
	length := uint32(len(segment)) //nolint:gosec // segments are at most 65535 bytes
	sum += length>>16 + length&0xffff
	sum += protoUDP
	add(segment)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum) //nolint:gosec // folded to 16 bits above
}
