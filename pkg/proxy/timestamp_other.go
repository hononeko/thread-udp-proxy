//go:build !linux

package proxy

import (
	"errors"
	"net"
	"time"
)

var oobSize = 0

// enableTimestamps is only implemented on Linux. Elsewhere latency is measured from the time a datagram is read.
func enableTimestamps(_ *net.UDPConn) error {
	return errors.New("kernel receive timestamps are only supported on Linux")
}

func kernelTimestamp(_ []byte) (time.Time, bool) {
	return time.Time{}, false
}
