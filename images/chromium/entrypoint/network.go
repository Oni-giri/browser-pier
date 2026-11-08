package main

import (
	"os"
	"strconv"
	"strings"
)

// Address flags from /proc/net/if_inet6 (linux/if_addr.h).
const (
	addrTentative = 0x40
	addrDADFailed = 0x08
)

// ipv6Settled reports whether the container's IPv6 addresses have finished
// duplicate address detection. Until then an address is tentative; when
// it stops being so, a second or two after the container starts, Chromium
// sees a new address, takes it for a network change and fails every
// request in flight with ERR_NETWORK_CHANGED.
func ipv6Settled() bool {
	b, err := os.ReadFile("/proc/net/if_inet6")
	if err != nil {
		return true // no IPv6
	}
	return addressesSettled(string(b))
}

func addressesSettled(ifInet6 string) bool {
	for _, line := range strings.Split(ifInet6, "\n") {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		flags, err := strconv.ParseUint(f[4], 16, 32)
		if err == nil && flags&addrTentative != 0 && flags&addrDADFailed == 0 {
			return false
		}
	}
	return true
}
