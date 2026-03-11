// Package netutil provides helpers for binding TCP listeners with automatic
// port fallback.
package netutil

import (
	"fmt"
	"net"
	"strconv"
)

// Listen tries to bind addr (e.g. ":18080").  If the port is already in use it
// increments the port number by one and retries, up to maxTries times.
// It returns the bound listener and the address it actually bound to.
func Listen(addr string, maxTries int) (net.Listener, string, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", fmt.Errorf("netutil: invalid addr %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, "", fmt.Errorf("netutil: non-numeric port in %q: %w", addr, err)
	}

	for i := 0; i < maxTries; i++ {
		candidate := net.JoinHostPort(host, strconv.Itoa(port+i))
		ln, err := net.Listen("tcp", candidate)
		if err == nil {
			if i > 0 {
				// Inform caller that the address shifted.
				_ = i // used below in log; callers see resolved addr
			}
			return ln, ln.Addr().String(), nil
		}
	}

	return nil, "", fmt.Errorf("netutil: no free port found starting at %s (tried %d)", addr, maxTries)
}
