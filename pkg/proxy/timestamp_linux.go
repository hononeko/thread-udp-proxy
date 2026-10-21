//go:build linux

package proxy

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// oobSize fits the one SCM_TIMESTAMPNS control message a socket with SO_TIMESTAMPNS receives per datagram.
var oobSize = unix.CmsgSpace(16)

// enableTimestamps makes the kernel attach its receive time to every datagram read from conn.
func enableTimestamps(conn *net.UDPConn) error {
	rc, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("failed to access the socket: %w", err)
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1) //nolint:gosec // a file descriptor fits in an int
	}); err != nil {
		return fmt.Errorf("failed to access the socket: %w", err)
	}
	if serr != nil {
		return fmt.Errorf("failed to enable SO_TIMESTAMPNS: %w", serr)
	}
	return nil
}

// kernelTimestamp returns the receive time from the control messages of a read on a socket with SO_TIMESTAMPNS.
func kernelTimestamp(oob []byte) (time.Time, bool) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return time.Time{}, false
	}
	for _, m := range msgs {
		// A struct timespec of two 64-bit fields, as on the 64-bit platforms the image is built for. A shorter one
		// (32-bit time_t) isn't decoded, and the caller falls back to the read time.
		if m.Header.Level == unix.SOL_SOCKET && m.Header.Type == unix.SCM_TIMESTAMPNS && len(m.Data) >= 16 {
			sec := int64(binary.NativeEndian.Uint64(m.Data[0:8]))   //nolint:gosec // time_t is signed
			nsec := int64(binary.NativeEndian.Uint64(m.Data[8:16])) //nolint:gosec // tv_nsec is signed
			return time.Unix(sec, nsec), true
		}
	}
	return time.Time{}, false
}
