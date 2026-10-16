//go:build linux

package proxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

type rawConn struct {
	fd      int
	ifindex int
	hw      net.HardwareAddr
}

// Listen opens an AF_PACKET socket that reads the IPv6 frames addressed to iface's MAC and writes frames out of
// iface. It needs CAP_NET_RAW. Reads return ErrTimeout after readTimeout without a frame.
func Listen(iface string, readTimeout time.Duration) (FrameConn, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("failed to find interface %q: %w", iface, err)
	}
	if len(ifi.HardwareAddr) != 6 {
		return nil, fmt.Errorf("interface %q has no Ethernet address", iface)
	}

	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, int(htons(syscall.ETH_P_IPV6)))
	if err != nil {
		return nil, fmt.Errorf("failed to open a raw packet socket (needs CAP_NET_RAW): %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: htons(syscall.ETH_P_IPV6), Ifindex: ifi.Index}); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("failed to bind the raw packet socket to %q: %w", iface, err)
	}
	tv := syscall.NsecToTimeval(readTimeout.Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("failed to set the read timeout: %w", err)
	}
	return &rawConn{fd: fd, ifindex: ifi.Index, hw: ifi.HardwareAddr}, nil
}

func (c *rawConn) ReadFrame(buf []byte) (int, error) {
	for {
		n, from, err := syscall.Recvfrom(c.fd, buf, 0)
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
			return 0, ErrTimeout
		}
		if err != nil {
			return 0, err
		}
		// Only frames sent to this host. A packet socket also sees the host's own outgoing frames, including the
		// replies this proxy writes, which would otherwise be relayed again if a client used the relayed port.
		if ll, ok := from.(*syscall.SockaddrLinklayer); ok && ll.Pkttype != syscall.PACKET_HOST {
			continue
		}
		return n, nil
	}
}

func (c *rawConn) WriteFrame(frame []byte) error {
	return syscall.Sendto(c.fd, frame, 0, &syscall.SockaddrLinklayer{Protocol: htons(syscall.ETH_P_IPV6), Ifindex: c.ifindex})
}

func (c *rawConn) HardwareAddr() net.HardwareAddr {
	return c.hw
}

func (c *rawConn) Close() error {
	return syscall.Close(c.fd)
}

// htons converts v to network byte order, as packet sockets expect protocol numbers.
func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}
