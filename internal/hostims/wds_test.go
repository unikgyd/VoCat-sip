package hostims

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"testing"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

func runtimePacket(tlvs ...qmi.TLV) *qmi.Packet {
	return &qmi.Packet{ServiceType: qmi.ServiceWDS, ClientID: 7, MessageID: qmi.WDSGetRuntimeSettings,
		TLVs: append([]qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}}, tlvs...)}
}

func validRuntimePacket() *qmi.Packet {
	v6 := netip.MustParseAddr("2001:db8::1").As16()
	pcscf6 := netip.MustParseAddr("2001:db8::2").As16()
	return runtimePacket(qmi.TLV{Type: 0x14, Value: []byte("ims")},
		qmi.TLV{Type: 0x1E, Value: []byte{1, 2, 0, 192}},
		qmi.TLV{Type: 0x21, Value: []byte{255, 255, 255, 255}},
		qmi.TLV{Type: 0x1F, Value: []byte{0, 9}},
		qmi.TLV{Type: 0x23, Value: []byte{2, 1, 100, 51, 198, 1, 100, 51, 198}},
		qmi.TLV{Type: 0x25, Value: append(v6[:], 64)},
		qmi.TLV{Type: 0x2E, Value: append([]byte{1}, pcscf6[:]...)},
		qmi.NewTLVUint32(0x29, 1500), qmi.NewTLVUint8(0x2B, 4))
}

func TestWDSRuntimeBindsExactAPNAndMatchingFamilies(t *testing.T) {
	runtime, err := parseRuntime(validRuntimePacket())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Profile == nil || runtime.Profile.Index != 9 || runtime.Profile.Type != 0 || runtime.APN != "ims" || runtime.MTU != 1500 {
		t.Fatalf("runtime metadata = %#v", runtime)
	}
	if runtime.IPv4.String() != "192.0.2.1/32" || runtime.IPv6.String() != "2001:db8::1/64" || len(runtime.PCSCF) != 2 {
		t.Fatalf("runtime addressing = %#v", runtime)
	}
	endpoints, err := runtime.Endpoints("IMS")
	if err != nil || len(endpoints) != 2 || endpoints[0].Remote.String() != "198.51.100.1" || endpoints[1].Remote.String() != "2001:db8::2" {
		t.Fatalf("endpoints=%v,err=%v", endpoints, err)
	}
	if _, err := runtime.Endpoints("internet"); !errors.Is(err, ErrPCSCF) {
		t.Fatal("another APN was accepted")
	}
	runtime.IPv6 = netip.Prefix{}
	endpoints, err = runtime.Endpoints("ims")
	if err != nil || len(endpoints) != 1 || !endpoints[0].Local.Is4() {
		t.Fatal("unavailable address family was used")
	}
}

func TestWDSRuntimeDoesNotUseDNSAsPCSCF(t *testing.T) {
	runtime, err := parseRuntime(runtimePacket(qmi.TLV{Type: 0x14, Value: []byte("ims")},
		qmi.TLV{Type: 0x1E, Value: []byte{1, 2, 0, 192}}, qmi.TLV{Type: 0x21, Value: []byte{255, 255, 255, 255}},
		qmi.TLV{Type: 0x15, Value: []byte{53, 113, 0, 203}}, qmi.TLV{Type: 0x20, Value: []byte{2, 2, 0, 192}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Endpoints("ims"); !errors.Is(err, ErrPCSCF) {
		t.Fatal("DNS/gateway became P-CSCF")
	}
}

func TestWDSRuntimeRejectsMalformedSingletons(t *testing.T) {
	for tag, expected := range map[byte]int{2: 4, 0x1E: 4, 0x20: 4, 0x21: 4, 0x1F: 2, 0x22: 1, 0x25: 17, 0x26: 17, 0x29: 4, 0x2B: 1} {
		for _, length := range []int{0, expected - 1, expected + 1} {
			t.Run(fmt.Sprintf("%02x/%d", tag, length), func(t *testing.T) {
				packet := runtimePacket(qmi.TLV{Type: tag, Value: make([]byte, length)})
				if tag == 2 {
					packet.TLVs = packet.TLVs[1:]
				}
				if _, err := parseRuntime(packet); !errors.Is(err, ErrRuntime) {
					t.Fatalf("tag=%x,length=%d accepted", tag, length)
				}
			})
		}
		packet := runtimePacket(qmi.TLV{Type: tag, Value: make([]byte, expected)}, qmi.TLV{Type: tag, Value: make([]byte, expected)})
		if _, err := parseRuntime(packet); !errors.Is(err, ErrRuntime) {
			t.Fatalf("duplicate tag=%x accepted", tag)
		}
	}
}

func TestWDSRuntimeRejectsMalformedListsAndAddresses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tag   byte
		value []byte
	}{
		{"v4-empty", 0x23, nil}, {"v4-short", 0x23, []byte{1}}, {"v4-tail", 0x23, []byte{0, 0}},
		{"v6-short", 0x2e, []byte{1, 0}}, {"v6-tail", 0x2e, []byte{0, 0}},
		{"too-many", 0x23, append([]byte{9}, make([]byte, 36)...)},
		{"v4-multicast", 0x23, []byte{1, 1, 0, 0, 224}}, {"v4-unspecified", 0x23, []byte{1, 0, 0, 0, 0}},
		{"v4-loopback", 0x23, []byte{1, 1, 0, 0, 127}},
		{"v6-bad-prefix", 0x25, append(make([]byte, 16), 129)},
		{"apn-injection", 0x14, []byte("ims\r\nprivate")},
		{"family", 0x2b, []byte{8}}, {"mtu", 0x29, []byte{1, 0, 0, 0}},
		{"domain-short", 0x24, []byte{1, 3, 0, 'i'}}, {"domain-tail", 0x24, []byte{0, 0}},
		{"domain-injection", 0x24, []byte{1, 3, 0, 'i', '\n', 'm'}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseRuntime(runtimePacket(qmi.TLV{Type: tc.tag, Value: tc.value})); !errors.Is(err, ErrRuntime) {
				t.Fatal("malformed runtime accepted")
			}
		})
	}
	if _, err := parseRuntime(runtimePacket(qmi.TLV{Type: 0x1E, Value: []byte{1, 2, 0, 192}}, qmi.TLV{Type: 0x21, Value: []byte{0, 255, 0, 255}})); err == nil {
		t.Fatal("noncontiguous subnet accepted")
	}
	if _, err := parseRuntime(runtimePacket(qmi.TLV{Type: 0x1E, Value: []byte{1, 2, 0, 192}})); err == nil {
		t.Fatal("unknown subnet became /32")
	}
}

func TestWDSRuntimeRetainsDomainsWithoutResolvingThem(t *testing.T) {
	domain := "PcScf.Example.Invalid"
	value := append([]byte{1, byte(len(domain)), 0}, []byte(domain)...)
	runtime, err := parseRuntime(runtimePacket(qmi.TLV{Type: 0x24, Value: value}))
	if err != nil || !reflect.DeepEqual(runtime.PCSCFDomains, []string{"pcscf.example.invalid"}) || len(runtime.PCSCF) != 0 {
		t.Fatal("domain parsing failed")
	}
}

type fakeSender struct {
	packet *qmi.Packet
	calls  int
	err    error
	t      *testing.T
}

func (sender *fakeSender) SendRequest(_ context.Context, service, cid uint8, message uint16, tlvs []qmi.TLV) (*qmi.Packet, error) {
	sender.calls++
	if service != qmi.ServiceWDS || cid != 7 || message != qmi.WDSGetRuntimeSettings || len(tlvs) != 1 || tlvs[0].Type != 0x10 {
		sender.t.Fatal("reader mutated or used an unowned CID")
	}
	mask := binary.LittleEndian.Uint32(tlvs[0].Value)
	for _, bit := range []uint32{qmi.RuntimeMaskAPNName, qmi.RuntimeMaskPCSCFAddr, qmi.RuntimeMaskPCSCFDomain, qmi.RuntimeMaskIPAddr} {
		if mask&bit == 0 {
			sender.t.Fatal("missing IMS discovery setting")
		}
	}
	return sender.packet, sender.err
}

func TestReadWDSRuntimeUsesOnlyOwnedCIDAndCorrelatedResponse(t *testing.T) {
	sender := &fakeSender{packet: validRuntimePacket(), t: t}
	if _, err := ReadRuntime(context.Background(), sender, 7); err != nil || sender.calls != 1 {
		t.Fatal("valid read failed")
	}
	for _, cid := range []byte{0, 255} {
		if _, err := ReadRuntime(context.Background(), sender, cid); err == nil {
			t.Fatal("unowned CID accepted")
		}
	}
	if sender.calls != 1 {
		t.Fatal("invalid CID caused I/O")
	}
	for _, name := range []string{"cid", "service", "message", "indication", "missing_result", "failure_result", "transport"} {
		sender.packet, sender.err = validRuntimePacket(), nil
		switch name {
		case "cid":
			sender.packet.ClientID++
		case "service":
			sender.packet.ServiceType++
		case "message":
			sender.packet.MessageID++
		case "indication":
			sender.packet.IsIndication = true
		case "missing_result":
			sender.packet.TLVs = sender.packet.TLVs[1:]
		case "failure_result":
			sender.packet.TLVs[0].Value = []byte{1, 0, 14, 0}
		case "transport":
			sender.err = errors.New("raw private subscriber payload")
		}
		if _, err := ReadRuntime(context.Background(), sender, 7); !errors.Is(err, ErrRuntime) {
			t.Fatalf("%s accepted", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := sender.calls
	if _, err := ReadRuntime(ctx, sender, 7); err == nil || sender.calls != before {
		t.Fatal("cancelled read performed I/O")
	}
}

func FuzzWDSRuntimeLists(f *testing.F) {
	for _, seed := range [][]byte{nil, {0}, {1, 0, 0, 0, 0}, {1, 1, 100, 51, 198}, {0xff}} {
		f.Add(byte(0x23), seed)
	}
	f.Fuzz(func(t *testing.T, tag byte, value []byte) {
		_, _ = parseRuntime(runtimePacket(qmi.TLV{Type: tag, Value: value}))
	})
}
