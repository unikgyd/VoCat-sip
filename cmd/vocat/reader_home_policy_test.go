package main

import "testing"

func TestReaderHomeApplicationEnablesPairedPCSCAndNativeQMI(t *testing.T) {
	if !voWiFiSIMReaderOptions().AllowUnresolvedMNCLength {
		t.Fatal("application reader policy was not enabled")
	}
}
