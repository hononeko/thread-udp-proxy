//go:build !linux

package proxy

import (
	"errors"
	"syscall"
)

// bindToInterface is only implemented on Linux.
func bindToInterface(_ syscall.RawConn, _ int) error {
	return errors.New("binding a socket to an interface is only supported on Linux")
}
