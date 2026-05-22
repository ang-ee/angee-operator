package service

import (
	"errors"
	"net"
	"strconv"
	"syscall"
)

// hostPortUnavailable reports whether the given TCP port is in use on
// the host. The tcp4 wildcard probe is authoritative because docker
// publishes container ports to 0.0.0.0 by default. The tcp6 probe is
// advisory: a tcp6 listener on a dual-stack kernel would also block
// docker, but on hosts where IPv6 is disabled (EAFNOSUPPORT) every
// tcp6 listen fails and we'd otherwise report the entire pool as
// taken.
func hostPortUnavailable(port int) bool {
	portText := strconv.Itoa(port)
	ln, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", portText))
	if err != nil {
		return true
	}
	_ = ln.Close()
	ln6, err := net.Listen("tcp6", net.JoinHostPort("::", portText))
	if err != nil {
		return !errors.Is(err, syscall.EAFNOSUPPORT)
	}
	_ = ln6.Close()
	return false
}
