package hostims

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"reflect"
	"testing"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

type senderFunc func(context.Context, byte, byte, uint16, []qmi.TLV) (*qmi.Packet, error)

func (f senderFunc) SendRequest(ctx context.Context, service, cid byte, message uint16, tlvs []qmi.TLV) (*qmi.Packet, error) {
	return f(ctx, service, cid, message, tlvs)
}

func boundRuntimeRow(address BearerAddress) Runtime {
	r := Runtime{APN: "ims", Family: 6, MTU: address.MTU, PCSCF: []netip.Addr{netip.MustParseAddr("2001:db8::20")}}
	if address.Address.Addr().Is4() {
		r.Family = 4
		r.IPv4 = address.Address
		r.Gateway4 = address.Gateway
		r.PCSCF = []netip.Addr{netip.MustParseAddr("198.51.100.20")}
	} else {
		r.IPv6 = address.Address
		r.Gateway6 = address.Gateway
	}
	return r
}
func boundRuntimeTLVs(r Runtime) []qmi.TLV {
	values := []qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}, {Type: 0x14, Value: []byte(r.APN)}, {Type: 0x2b, Value: []byte{r.Family}}}
	if r.IPv4.IsValid() {
		ip := r.IPv4.Addr().As4()
		mask := uint32(0)
		if r.IPv4.Bits() > 0 {
			mask = ^uint32(0) << (32 - r.IPv4.Bits())
		}
		values = append(values, qmi.TLV{Type: 0x1e, Value: []byte{ip[3], ip[2], ip[1], ip[0]}}, qmi.NewTLVUint32(0x21, mask))
	}
	if r.IPv6.IsValid() {
		ip := r.IPv6.Addr().As16()
		values = append(values, qmi.TLV{Type: 0x25, Value: append(ip[:], byte(r.IPv6.Bits()))})
	}
	if r.MTU != 0 {
		values = append(values, qmi.NewTLVUint32(0x29, r.MTU))
	}
	var v4, v6 []byte
	for _, ip := range r.PCSCF {
		if ip.Is4() {
			a := ip.As4()
			v4 = append(v4, a[3], a[2], a[1], a[0])
		} else {
			v6 = append(v6, ip.AsSlice()...)
		}
	}
	if len(v4) > 0 {
		values = append(values, qmi.TLV{Type: 0x23, Value: append([]byte{byte(len(v4) / 4)}, v4...)})
	}
	if len(v6) > 0 {
		values = append(values, qmi.TLV{Type: 0x2e, Value: append([]byte{byte(len(v6) / 16)}, v6...)})
	}
	return values
}

func TestOwnedWDSWireBindsBeforeFamilyAndNeverStartsNetwork(t *testing.T) {
	var messages []uint16
	row := boundRuntimeRow(BearerAddress{Address: netip.MustParsePrefix("192.0.2.10/32"), MTU: 1500})
	sender := senderFunc(func(_ context.Context, service, cid byte, message uint16, input []qmi.TLV) (*qmi.Packet, error) {
		if service != qmi.ServiceWDS || cid != 7 {
			t.Fatal("foreign service/CID")
		}
		messages = append(messages, message)
		output := []qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}}
		switch message {
		case 0xa2:
			if len(input) != 2 || input[0].Type != 0x10 || len(input[0].Value) != 8 || binary.LittleEndian.Uint32(input[0].Value) != 2 || binary.LittleEndian.Uint32(input[0].Value[4:]) != 4 || input[1].Type != 0x11 || !reflect.DeepEqual(input[1].Value, []byte{11}) {
				t.Fatal("wrong endpoint/mux")
			}
		case 0x4d:
			if len(input) != 1 || input[0].Type != 1 || !reflect.DeepEqual(input[0].Value, []byte{4}) {
				t.Fatal("wrong scoped family")
			}
		case 0x22:
			output = append(output, qmi.TLV{Type: 1, Value: []byte{2}})
		case qmi.WDSGetRuntimeSettings:
			output = boundRuntimeTLVs(row)
		default:
			t.Fatal("unexpected mutating request", message)
		}
		return &qmi.Packet{ServiceType: service, ClientID: cid, MessageID: message, TLVs: output}, nil
	})
	got, err := readBoundRuntime(context.Background(), sender, 7, wdsBinding{Endpoint: 2, Interface: 4, Mux: 11}, 4, func(context.Context) error { return nil })
	if err != nil || got.IPv4 != row.IPv4 {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(messages, []uint16{0xa2, 0x4d, 0x22, qmi.WDSGetRuntimeSettings, 0x22}) {
		t.Fatal("wrong query order", messages)
	}
}

func TestOwnedWDSWireRejectsUnconnectedOrMalformedState(t *testing.T) {
	for _, mode := range []string{"disconnected", "suspended", "missing", "extra", "duplicate", "wrong-cid", "wrong-family", "out-of-call", "bind-unknown"} {
		t.Run(mode, func(t *testing.T) {
			queries := 0
			sender := senderFunc(func(_ context.Context, service, cid byte, message uint16, _ []qmi.TLV) (*qmi.Packet, error) {
				if mode == "bind-unknown" && message == 0xa2 {
					return nil, errors.New("private modem failure")
				}
				output := []qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}}
				if message == 0x22 {
					status := []byte{2}
					switch mode {
					case "disconnected":
						status = []byte{1}
					case "suspended":
						status = []byte{3}
					case "missing":
						status = nil
					case "extra":
						status = []byte{2, 0}
					}
					if status != nil {
						output = append(output, qmi.TLV{Type: 1, Value: status})
					}
					if mode == "duplicate" {
						output = append(output, qmi.TLV{Type: 1, Value: []byte{2}})
					}
				}
				if message == qmi.WDSGetRuntimeSettings {
					queries++
					row := boundRuntimeRow(BearerAddress{Address: netip.MustParsePrefix("192.0.2.10/32")})
					if mode == "wrong-family" {
						row.Family = 6
					}
					output = boundRuntimeTLVs(row)
					if mode == "out-of-call" {
						output = []qmi.TLV{{Type: 2, Value: []byte{1, 0, 15, 0}}}
					}
				}
				if mode == "wrong-cid" {
					cid++
				}
				return &qmi.Packet{ServiceType: service, ClientID: cid, MessageID: message, TLVs: output}, nil
			})
			if _, err := readBoundRuntime(context.Background(), sender, 7, wdsBinding{Endpoint: 2, Interface: 4, Mux: 11}, 4, func(context.Context) error { return nil }); err == nil {
				t.Fatal("bad state accepted")
			}
			if mode == "disconnected" && queries != 0 {
				t.Fatal("unconnected call still read")
			}
		})
	}
}

func TestOwnedWDSMergeRequiresBothFamilyProofs(t *testing.T) {
	configuration := BearerConfiguration{Interface: "wwan-test", Addresses: []BearerAddress{{Address: netip.MustParsePrefix("192.0.2.10/32"), MTU: 1500}, {Address: netip.MustParsePrefix("2001:db8::10/64"), MTU: 1500}}}
	rows := []Runtime{boundRuntimeRow(configuration.Addresses[0]), boundRuntimeRow(configuration.Addresses[1])}
	// Some firmware reports both P-CSCF lists in only one family's settings.
	rows[0].PCSCF = append(rows[0].PCSCF, rows[1].PCSCF...)
	rows[1].PCSCF = nil
	merged, err := mergeBoundRuntimes(rows, configuration, "ims")
	if err != nil || len(merged.PCSCF) != 2 {
		t.Fatal("cross-family list lost despite both proven locals", err)
	}
	for _, mode := range []string{"apn", "prefix", "address", "gateway", "mtu", "family"} {
		t.Run(mode, func(t *testing.T) {
			copy := []Runtime{cloneRuntime(rows[0]), cloneRuntime(rows[1])}
			switch mode {
			case "apn":
				copy[1].APN = "internet"
			case "prefix":
				copy[1].IPv6 = netip.MustParsePrefix("2001:db8::10/128")
			case "address":
				copy[1].IPv6 = netip.MustParsePrefix("2001:db8::99/64")
			case "gateway":
				copy[1].Gateway6 = netip.MustParseAddr("2001:db8::ff")
			case "mtu":
				copy[1].MTU = 1400
			case "family":
				copy[1].Family = 4
			}
			if _, err := mergeBoundRuntimes(copy, configuration, "ims"); err == nil {
				t.Fatal("mismatched proof accepted")
			}
		})
	}
}

func TestOwnedWDSCIDNeverAcceptsAnotherServiceOrRetries(t *testing.T) {
	for _, mode := range []string{"wrong-service", "wrong-cid", "extra", "lost", "negative", "valid"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			sender := senderFunc(func(_ context.Context, service, cid byte, message uint16, input []qmi.TLV) (*qmi.Packet, error) {
				calls++
				if calls > 1 {
					t.Fatal("CTL retried")
				}
				if len(input) != 1 || !reflect.DeepEqual(input[0].Value, []byte{qmi.ServiceWDS}) {
					t.Fatal("wrong service allocation")
				}
				if mode == "lost" {
					return nil, errors.New("lost private reply")
				}
				info := []byte{qmi.ServiceWDS, 7}
				status := []byte{0, 0, 0, 0}
				switch mode {
				case "wrong-service":
					info[0] = qmi.ServiceUIM
				case "wrong-cid":
					info[1] = 255
				case "extra":
					info = append(info, 0)
				case "negative":
					status = []byte{1, 0, 5, 0}
				}
				return &qmi.Packet{ServiceType: service, ClientID: cid, MessageID: message, TLVs: []qmi.TLV{{Type: 2, Value: status}, {Type: 1, Value: info}}}, nil
			})
			cid, err := ownedServiceCID(context.Background(), sender, qmi.ServiceWDS, 0)
			if mode == "valid" {
				if err != nil || cid != 7 {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("bad allocation accepted")
			}
		})
	}
}
