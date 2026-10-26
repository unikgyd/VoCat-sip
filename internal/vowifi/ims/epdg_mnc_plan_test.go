package ims

import (
	"testing"

	"vocat/internal/vowifi"
)

func TestMNCPlanSelectedHomeReachesActualSIPIdentityBuilder(t *testing.T) {
	id := vowifi.SIMIdentity{ICCID: "synthetic", IMSI: "999123123456789", HomeMCC: "999", HomeMNC: "12", HomeMNCUncertain: true}
	plans, err := vowifi.BuildEPDGAttempts(id, vowifi.EPDGDiscoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range plans {
		got, err := deriveIdentities(p.Identity, Config{})
		if err != nil {
			t.Fatal(err)
		}
		mnc := p.Identity.HomeMNC
		for len(mnc) < 3 {
			mnc = "0" + mnc
		}
		want := "ims.mnc" + mnc + ".mcc999.3gppnetwork.org"
		if got.domain != want || got.private != id.IMSI+"@"+want || got.public != "sip:"+id.IMSI+"@"+want {
			t.Fatalf("split SIP realm: %+v", got)
		}
		seen[p.Identity.HomeMNC] = true
	}
	if !seen["12"] || !seen["123"] {
		t.Fatal("one legal IMS realm missing")
	}
}
