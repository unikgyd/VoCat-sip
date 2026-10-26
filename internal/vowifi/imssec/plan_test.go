package imssec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func planConfig() Config {
	return Config{LocalIP: net.ParseIP("192.0.2.10"), RemoteIP: net.ParseIP("192.0.2.20"),
		UEClientSPI: 1001, UEServerSPI: 1002, PCSCFClientSPI: 2001, PCSCFServerSPI: 2002,
		UEClientPort: 40666, UEServerPort: 55610, PCSCFClientPort: 50602, PCSCFServerPort: 50600,
		EncryptionKey: bytes.Repeat([]byte{0xab}, 16), IntegrityKey: bytes.Repeat([]byte{0xcd}, 20)}
}

func TestPlanFourStatesAndAllEightExactTCPUDPPolicies(t *testing.T) {
	for _, v6 := range []bool{false, true} {
		t.Run(fmt.Sprint(v6), func(t *testing.T) {
			config := planConfig()
			if v6 {
				config.LocalIP = net.ParseIP("2001:db8::10")
				config.RemoteIP = net.ParseIP("2001:db8::20")
			}
			plan, err := BuildPlan(config)
			if err != nil || ValidatePlan(config, plan) != nil {
				t.Fatal("valid plan rejected", err)
			}
			local, _ := netip.AddrFromSlice(config.LocalIP)
			remote, _ := netip.AddrFromSlice(config.RemoteIP)
			local, remote = local.Unmap(), remote.Unmap()
			client, server := ClientPairReqID(config), ServerPairReqID(config)
			if client == 0 || server == 0 || client == server {
				t.Fatal("reqid pair collision")
			}
			wantStates := [4]State{
				{"outbound UE-client to P-CSCF-server state", local, remote, 2002, client},
				{"inbound P-CSCF-server to UE-client state", remote, local, 1001, client},
				{"inbound P-CSCF-client to UE-server state", remote, local, 1002, server},
				{"outbound UE-server to P-CSCF-client state", local, remote, 2001, server},
			}
			if plan.States != wantStates {
				t.Fatal("wrong SPI direction, pair, or state order")
			}
			for index, policy := range plan.Policies {
				group := index / 2
				protocol := uint8(6)
				if index%2 == 1 {
					protocol = 17
				}
				wantState := wantStates[group]
				wantSrc, wantDst := wantState.Src, wantState.Dst
				wantSrcPort := [4]uint16{40666, 50600, 0, 55610}[group]
				wantDstPort := [4]uint16{50600, 40666, 55610, 0}[group]
				wantDirection := [4]string{"out", "in", "in", "out"}[group]
				if policy.Protocol != protocol || policy.Src != netip.PrefixFrom(wantSrc, wantSrc.BitLen()) || policy.Dst != netip.PrefixFrom(wantDst, wantDst.BitLen()) ||
					policy.SrcPort != wantSrcPort || policy.DstPort != wantDstPort || policy.Direction != wantDirection ||
					policy.TemplateSrc != wantState.Src || policy.TemplateDst != wantState.Dst || policy.SPI != wantState.SPI || policy.ReqID != wantState.ReqID {
					t.Fatal("wrong TCP/UDP selector, wildcard or template", index)
				}
			}
		})
	}
}

func TestPlanScopedReqIDsAreExplicitAndDoNotChangeSelectors(t *testing.T) {
	config := planConfig()
	legacy, _ := BuildPlan(config)
	for _, pair := range [][2]uint32{{7, 8}, {0xffffffff, 0x80000000}, {1, 2}} {
		plan, err := BuildScopedPlan(config, pair[0], pair[1])
		if err != nil || ValidateScopedPlan(config, pair[0], pair[1], plan) != nil {
			t.Fatal("valid owner scope rejected", err)
		}
		for i, state := range plan.States {
			want := legacy.States[i]
			want.ReqID = pair[i/2]
			if state != want {
				t.Fatal("scope changed canonical state instead of reqid")
			}
		}
		for i, policy := range plan.Policies {
			want := legacy.Policies[i]
			want.ReqID = pair[i/4]
			if policy != want {
				t.Fatal("scope changed canonical selector instead of reqid")
			}
		}
		if pair != ([2]uint32{ClientPairReqID(config), ServerPairReqID(config)}) && ValidatePlan(config, plan) == nil {
			t.Fatal("scoped plan accepted as legacy")
		}
	}
	for _, pair := range [][2]uint32{{0, 1}, {1, 0}, {5, 5}} {
		if plan, err := BuildScopedPlan(config, pair[0], pair[1]); err == nil || plan != (Plan{}) {
			t.Fatal("invalid owner reqid scope exposed plan")
		}
	}
}

func TestPlanRejectsWidenedOrMissingPolicyAndWrongStateBindings(t *testing.T) {
	config := planConfig()
	for _, mode := range []string{"udp-missing", "wildcard-client-port", "wildcard-address", "wrong-template", "wrong-direction", "wrong-spi", "wrong-reqid", "reordered"} {
		t.Run(mode, func(t *testing.T) {
			plan, _ := BuildPlan(config)
			switch mode {
			case "udp-missing":
				plan.Policies[3].Protocol = 6
			case "wildcard-client-port":
				plan.Policies[3].DstPort = 0
			case "wildcard-address":
				plan.Policies[0].Dst = netip.MustParsePrefix("0.0.0.0/0")
			case "wrong-template":
				plan.Policies[0].TemplateDst = netip.MustParseAddr("192.0.2.99")
			case "wrong-direction":
				plan.Policies[3].Direction = "out"
			case "wrong-spi":
				plan.States[0].SPI = config.UEClientSPI
			case "wrong-reqid":
				plan.States[0].ReqID = ServerPairReqID(config)
			case "reordered":
				plan.Policies[0], plan.Policies[1] = plan.Policies[1], plan.Policies[0]
			}
			if ValidatePlan(config, plan) == nil {
				t.Fatal("altered plan accepted")
			}
		})
	}
}

func TestPlanConfigValidationAndNormalizedAlgorithms(t *testing.T) {
	for _, mode := range []string{"nil-local", "malformed-local", "zero-v4", "zero-v6", "mapped-zero", "mixed-family", "zero-spi", "duplicate-spi", "low-port", "standard-port", "wide-port", "duplicate-client-server-port", "encryption", "integrity", "encryption-length", "integrity-length"} {
		t.Run(mode, func(t *testing.T) {
			config := planConfig()
			switch mode {
			case "nil-local":
				config.LocalIP = nil
			case "malformed-local":
				config.LocalIP = net.IP{1, 2, 3}
			case "zero-v4":
				config.LocalIP = net.IP{0, 0, 0, 0}
			case "zero-v6":
				config.LocalIP = net.ParseIP("::")
			case "mapped-zero":
				config.LocalIP = net.ParseIP("0.0.0.0")
			case "mixed-family":
				config.RemoteIP = net.ParseIP("2001:db8::20")
			case "zero-spi":
				config.UEClientSPI = 0
			case "duplicate-spi":
				config.PCSCFServerSPI = config.UEClientSPI
			case "low-port":
				config.UEClientPort = 1024
			case "standard-port":
				config.PCSCFServerPort = 5060
			case "wide-port":
				config.PCSCFServerPort = 65536
			case "duplicate-client-server-port":
				config.UEClientPort = config.UEServerPort
			case "encryption":
				config.EncryptionAlgorithm = "secret-invalid-algorithm"
			case "integrity":
				config.IntegrityAlgorithm = "secret-invalid-algorithm"
			case "encryption-length":
				config.EncryptionKey = nil
			case "integrity-length":
				config.IntegrityKey = nil
			}
			if plan, err := BuildPlan(config); err == nil || plan != (Plan{}) {
				t.Fatal("invalid agreement exposed plan")
			} else if strings.Contains(err.Error(), "secret-invalid") {
				t.Fatal("untrusted algorithm leaked")
			}
		})
	}
	for _, row := range []struct {
		enc, auth           string
		encBytes, authBytes int
	}{{"null", "hmac-md5-96", 0, 16}, {" AES-CBC ", " HMAC-SHA-1-96 ", 16, 20}, {"des-ede3-cbc", "hmac-md5-96", 24, 16}} {
		config := planConfig()
		config.EncryptionAlgorithm = row.enc
		config.IntegrityAlgorithm = row.auth
		config.EncryptionKey = make([]byte, row.encBytes)
		config.IntegrityKey = make([]byte, row.authBytes)
		if _, err := BuildPlan(config); err != nil {
			t.Fatal("supported negotiated transforms rejected", err)
		}
	}
}

func TestPlanDetachedFromMutableConfigAndContainsNoKeys(t *testing.T) {
	config := planConfig()
	copy := CloneConfig(config)
	plan, err := BuildPlan(config)
	if err != nil {
		t.Fatal(err)
	}
	before := plan
	clear(config.LocalIP)
	clear(config.RemoteIP)
	clear(config.EncryptionKey)
	clear(config.IntegrityKey)
	if plan != before || ValidatePlan(copy, plan) != nil || bytes.Equal(copy.EncryptionKey, config.EncryptionKey) || bytes.Equal(copy.LocalIP, config.LocalIP) {
		t.Fatal("plan or clone aliases agreement memory")
	}
	encoded, err := json.Marshal(plan)
	if err != nil || strings.Contains(string(encoded), "q6urq6ur") || strings.Contains(string(encoded), "zc3Nzc3N") {
		t.Fatal("plan contains encryption/integrity keys")
	}
	if fmt.Sprintf("%+v", copy) != "<private IMS security agreement>" {
		t.Fatal("agreement formatting leaks private fields")
	}
}

func FuzzPlanReqIDPairCollision(f *testing.F) {
	f.Add(uint32(1001), uint32(1002), uint32(2001), uint32(2002))
	f.Add(uint32(0x1000), uint32(0x2000), uint32(0x40002000), uint32(0x40001000))
	f.Add(uint32(1), uint32(2), uint32(0x80000002), uint32(0x80000001))
	f.Fuzz(func(t *testing.T, uc, us, pc, ps uint32) {
		config := planConfig()
		config.UEClientSPI = uc
		config.UEServerSPI = us
		config.PCSCFClientSPI = pc
		config.PCSCFServerSPI = ps
		client, server := ClientPairReqID(config), ServerPairReqID(config)
		if client == 0 || server == 0 || client == server {
			t.Fatal("derived reqid scope collision")
		}
		if plan, err := BuildPlan(config); err == nil && ValidatePlan(config, plan) != nil {
			t.Fatal("canonical plan fails exact validation")
		}
	})
}
