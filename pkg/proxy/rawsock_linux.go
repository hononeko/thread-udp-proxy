//go:build linux

package proxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"

	"github.com/hononeko/thread-udp-proxy/pkg/logger"
)

type rawConn struct {
	fd      int
	ifindex int
	hw      net.HardwareAddr
	// oob receives each frame's kernel timestamp. Only the single reader uses it.
	oob []byte
}

// Listen opens an AF_PACKET socket that reads the frames addressed to iface's MAC carrying UDP datagrams over IPv6 to
// port, and writes frames out of iface. It needs CAP_NET_RAW. Reads return ErrTimeout after readTimeout without a
// frame.
func Listen(iface string, port uint16, readTimeout time.Duration) (FrameConn, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("failed to find interface %q: %w", iface, err)
	}
	if len(ifi.HardwareAddr) != 6 {
		return nil, fmt.Errorf("interface %q has no Ethernet address", iface)
	}
	filter, err := assembleFilter(port)
	if err != nil {
		return nil, err
	}

	// Protocol 0 receives nothing until the bind below, so no frame gets in before the filter is attached.
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open a raw packet socket (needs CAP_NET_RAW): %w", err)
	}
	if err := unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, filter); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("failed to attach the socket filter: %w", err)
	}
	// Don't even queue the host's own outgoing frames (Linux 4.20+). ReadFrame skips them anyway, so an older kernel
	// only costs the copies.
	if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_IGNORE_OUTGOING, 1); err != nil {
		logger.Debug("Can't ignore outgoing frames in the kernel, skipping them in the proxy instead", "err", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IPV6), Ifindex: ifi.Index}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("failed to bind the raw packet socket to %q: %w", iface, err)
	}
	tv := unix.NsecToTimeval(readTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("failed to set the read timeout: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("failed to enable receive timestamps: %w", err)
	}
	return &rawConn{fd: fd, ifindex: ifi.Index, hw: ifi.HardwareAddr, oob: make([]byte, oobSize)}, nil
}

func (c *rawConn) ReadFrame(buf []byte) (int, time.Time, error) {
	for {
		n, oobn, _, from, err := unix.Recvmsg(c.fd, buf, c.oob, 0)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			return 0, time.Time{}, ErrTimeout
		}
		if err != nil {
			return 0, time.Time{}, err
		}
		// Only frames sent to this host. A packet socket also sees the host's own outgoing frames, including the
		// replies this proxy writes, which would otherwise be relayed again if a client used the relayed port.
		if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype != unix.PACKET_HOST {
			continue
		}
		rx, ok := kernelTimestamp(c.oob[:oobn])
		if !ok {
			rx = time.Now()
		}
		return n, rx, nil
	}
}

func (c *rawConn) WriteFrame(frame []byte) error {
	return unix.Sendto(c.fd, frame, 0, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IPV6), Ifindex: c.ifindex})
}

func (c *rawConn) HardwareAddr() net.HardwareAddr {
	return c.hw
}

func (c *rawConn) Close() error {
	return unix.Close(c.fd)
}

// assembleFilter compiles relayFilter for the kernel.
func assembleFilter(port uint16) (*unix.SockFprog, error) {
	raw, err := bpf.Assemble(relayFilter(port))
	if err != nil {
		return nil, fmt.Errorf("failed to assemble the socket filter: %w", err)
	}
	prog := make([]unix.SockFilter, len(raw))
	for i, ins := range raw {
		prog[i] = unix.SockFilter{Code: ins.Op, Jt: ins.Jt, Jf: ins.Jf, K: ins.K}
	}
	return &unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}, nil //nolint:gosec // a handful of instructions
}

// htons converts v to network byte order, as packet sockets expect protocol numbers.
func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}
