package vowifi

import (
	_ "embed"
	"encoding/json"
	"testing"
)

//go:embed testdata/epdg-discovery-v1.json
var epdgContract []byte

func TestSharedEPDGDiscoveryContract(t *testing.T) {
	var doc struct {
		Version int `json:"version"`
		Hosts   []struct {
			Input, Normalized string
			Invalid           bool
		}
		PLMNs []struct{ MCC, MNC, Host string }
	}
	if err := json.Unmarshal(epdgContract, &doc); err != nil || doc.Version != 1 || len(doc.Hosts) != 15 || len(doc.PLMNs) != 3 {
		t.Fatal("invalid contract", err)
	}
	for _, item := range doc.Hosts {
		got, err := NormalizeEPDGHost(item.Input)
		if (err != nil) != item.Invalid || !item.Invalid && got != item.Normalized {
			t.Fatalf("host %q: %q %v", item.Input, got, err)
		}
	}
	for _, item := range doc.PLMNs {
		if got := standardEPDGHostname(item.MCC, item.MNC); got != item.Host {
			t.Fatalf("PLMN result %q", got)
		}
	}
}
