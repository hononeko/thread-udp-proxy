//go:build !linux

package proxy

import (
	"errors"
	"time"
)

// Listen is only implemented on Linux, which has AF_PACKET sockets.
func Listen(_ string, _ time.Duration) (FrameConn, error) {
	return nil, errors.New("raw packet sockets are only supported on Linux")
}
