package ike

import (
	"strings"
	"testing"

	"vocat/internal/vowifi"
)

func TestMNCPlanSelectedHomeReachesActualEAPNAIBuilder(t *testing.T) {
	id := vowifi.SIMIdentity{ICCID: "synthetic", IMSI: "999123123456789", HomeMCC: "999", HomeMNC: "12", HomeMNCUncertain: true}
	plans, err := vowifi.BuildEPDGAttempts(id, vowifi.EPDGDiscoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range plans {
		got, err := permanentAKAIdentity(p.Identity)
		if err != nil {
			t.Fatal(err)
		}
		mnc := p.Identity.HomeMNC
		for len(mnc) < 3 {
			mnc = "0" + mnc
		}
		if string(got) != "0"+id.IMSI+"@nai.epc.mnc"+mnc+".mcc999.3gppnetwork.org" {
			t.Fatalf("split realm: %s", got)
		}
		if !strings.Contains(p.Candidate.Host, "mnc"+mnc+".") {
			t.Fatalf("wrong home host: %+v", p)
		}
		seen[p.Identity.HomeMNC] = true
	}
	if !seen["12"] || !seen["123"] {
		t.Fatal("one legal NAI missing")
	}
}
