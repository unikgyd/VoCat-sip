package main

import "testing"

func TestProfileIdentityApplicationEnablesProfileReadsWithoutBillableRestore(t *testing.T) {
	options := ec20VoWiFiAdapterOptions(nil)
	if !options.ReadProfileIdentities || !options.AllowUnresolvedMNCLength || options.RestoreCellularData {
		t.Fatal("profile source reading not connected to application policy")
	}
}
