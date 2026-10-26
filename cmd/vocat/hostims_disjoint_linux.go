//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/store"
)

func hostIMSPhysicalDisjoint(config store.Device, entries []device.Device, targetID string) bool {
	return hostIMSPhysicalDisjointVerifiedWith(config, entries, targetID, canonicalHostIMSPhysical, hostIMSDisjointEndpointUnder, hostIMSDisjointGeneration)
}

func hostIMSDisjointEndpointUnder(endpoint, physical string) bool {
	if !strings.HasPrefix(endpoint, "/dev/") || filepath.Clean(endpoint) != endpoint {
		return false
	}
	var before, after unix.Stat_t
	if unix.Stat(endpoint, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFCHR {
		return false
	}
	resolved, err := filepath.EvalSymlinks(fmt.Sprintf("/sys/dev/char/%d:%d", unix.Major(before.Rdev), unix.Minor(before.Rdev)))
	if err != nil || !strings.HasPrefix(resolved, physical+"/") {
		return false
	}
	if unix.Stat(endpoint, &after) != nil || after.Mode&unix.S_IFMT != unix.S_IFCHR || before.Dev != after.Dev || before.Ino != after.Ino || before.Rdev != after.Rdev {
		return false
	}
	return true
}
func hostIMSDisjointGeneration(candidate modem.Candidate, physical string) bool {
	if candidate.USBGeneration == "" {
		return true
	} // No epoch claim; current char topology remains mandatory.
	bus, busErr := os.ReadFile(filepath.Join(physical, "busnum"))
	number, numberErr := os.ReadFile(filepath.Join(physical, "devnum"))
	return busErr == nil && numberErr == nil && strings.TrimSpace(string(bus))+":"+strings.TrimSpace(string(number)) == candidate.USBGeneration
}
