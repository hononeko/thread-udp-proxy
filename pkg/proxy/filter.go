package proxy

import (
	"golang.org/x/net/bpf"

	"github.com/hononeko/thread-udp-proxy/pkg/packet"
)

// Offsets in an Ethernet frame carrying a UDP datagram directly over IPv6.
const (
	offEtherType  = 12
	offNextHeader = 14 + 6
	offUDPDstPort = 14 + 40 + 2

	protoUDP = 17
	// keepFrame is the filter's verdict for a frame it passes: deliver all of it.
	keepFrame = 0xffffffff
)

// relayFilter is a classic BPF program for the raw socket that only passes Ethernet frames carrying a UDP datagram
// directly over IPv6 to port, the only frames the proxy can relay. Everything else on the interface (neighbor
// discovery, mDNS, other traffic for the host) is dropped in the kernel instead of being copied to the proxy and parsed.
// A load past the end of a short frame also drops it. handleFrame still checks every frame the filter passes.
func relayFilter(port uint16) []bpf.Instruction {
	return []bpf.Instruction{
		bpf.LoadAbsolute{Off: offEtherType, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: packet.EtherTypeIPv6, SkipTrue: 5},
		bpf.LoadAbsolute{Off: offNextHeader, Size: 1},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: protoUDP, SkipTrue: 3},
		bpf.LoadAbsolute{Off: offUDPDstPort, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: uint32(port), SkipTrue: 1},
		bpf.RetConstant{Val: keepFrame},
		bpf.RetConstant{Val: 0},
	}
}
