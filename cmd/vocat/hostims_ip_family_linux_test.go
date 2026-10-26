//go:build linux

package main

import "testing"

func TestHostIMSUsesExplicitCardIPFamily(t *testing.T) {
	for input, want := range map[string]uint32{"": 0, "IP": 1, "IPV4": 1, "ipv6": 2, "IPV4V6": 4} {
		if got, err := hostIMSCardIPFamily(input); err != nil || got != want {
			t.Fatalf("IP family %q: %d %v", input, got, err)
		}
	}
	if _, err := hostIMSCardIPFamily("invalid"); err == nil {
		t.Fatal("invalid explicit IP family was silently replaced")
	}
}
