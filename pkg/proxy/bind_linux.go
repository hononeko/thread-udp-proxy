//go:build linux

package proxy

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// bindToInterface restricts the socket behind c to the interface with index ifindex: the kernel only routes what it
// sends through that interface, and only delivers it datagrams that arrived on it. Linux lets an unprivileged process
// do this to a socket that isn't bound to an interface yet (5.7+).
func bindToInterface(c syscall.RawConn, ifindex int) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BINDTOIFINDEX, ifindex) //nolint:gosec // a file descriptor fits in an int
	}); err != nil {
		return fmt.Errorf("failed to access the socket: %w", err)
	}
	if serr != nil {
		return fmt.Errorf("failed to bind the socket to interface %d: %w", ifindex, serr)
	}
	return nil
}
