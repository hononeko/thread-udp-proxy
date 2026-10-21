package proxy

import (
	"net/netip"
	"testing"

	"golang.org/x/net/bpf"

	"github.com/hononeko/thread-udp-proxy/pkg/packet"
)

func TestRelayFilter(t *testing.T) {
	const port = 5540
	matching := clientFrame(t, netip.MustParseAddrPort("[fd2f:741e:f59b:1::1]:5540"), 42557, "ping")
	mutate := func(off int, b ...byte) []byte {
		f := append([]byte(nil), matching...)
		copy(f[off:], b)
		return f
	}

	tests := []struct {
		name  string
		frame []byte
		pass  bool
	}{
		{"UDP over IPv6 to the relayed port", matching, true},
		{"empty datagram to the relayed port", clientFrame(t, netip.MustParseAddrPort("[fd2f:741e:f59b:1::1]:5540"), 1, ""), true},
		{"another UDP port (mDNS)", mutate(offUDPDstPort, 0x14, 0xe9), false},
		{"ICMPv6 (neighbor discovery)", mutate(offNextHeader, 58), false},
		{"IPv6 with a hop-by-hop extension header", mutate(offNextHeader, 0), false},
		{"IPv4", mutate(offEtherType, 0x08, 0x00), false},
		{"frame cut before the UDP header", matching[:offUDPDstPort], false},
		{"empty frame", nil, false},
	}

	vm, err := bpf.NewVM(relayFilter(port))
	if err != nil {
		t.Fatalf("the filter is not a valid program: %v", err)
	}
	if _, err := bpf.Assemble(relayFilter(port)); err != nil {
		t.Fatalf("the filter does not assemble: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kept, err := vm.Run(tt.frame)
			if err != nil {
				t.Fatal(err)
			}
			if pass := kept > 0; pass != tt.pass {
				t.Fatalf("filter kept %d bytes, want pass=%t", kept, tt.pass)
			}
			// The verdict is how many bytes to deliver; the kernel caps it at the frame's length.
			if tt.pass && kept < len(tt.frame) {
				t.Fatalf("filter kept %d of %d bytes, want the whole frame", kept, len(tt.frame))
			}
			// Everything the filter passes, the relay path can parse.
			if tt.pass {
				if _, err := packet.Parse(tt.frame); err != nil {
					t.Fatalf("passed a frame the relay can't parse: %v", err)
				}
			}
		})
	}
}
