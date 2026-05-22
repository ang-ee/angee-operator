package service

import (
	"net"
	"testing"
)

func TestHostPortUnavailableDetectsIPv6WildcardListener(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::]:0")
	if err != nil {
		t.Skipf("tcp6 wildcard listener unavailable on this host: %v", err)
	}
	defer func() { _ = ln.Close() }()

	port := ln.Addr().(*net.TCPAddr).Port
	if !hostPortUnavailable(port) {
		t.Fatalf("hostPortUnavailable(%d) = false, want true for tcp6 wildcard listener", port)
	}
}
