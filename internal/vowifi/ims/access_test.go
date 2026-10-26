package ims

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"vocat/internal/vowifi"
	"vocat/internal/vowifi/imssec"
)

type fakeCellularBearer struct {
	mu         sync.Mutex
	evidence   vowifi.CellularIMSEvidence
	err        error
	verified   []vowifi.SIMIdentity
	failAt     int
	bindings   []socketBinding
	bindErr    error
	protected  []imssec.SocketBinding
	protectErr error
	installer  IPSecSAInstaller
	retained   []IPSecSAHandle
}

func (bearer *fakeCellularBearer) RetainIPSec(handle imssec.Handle) {
	bearer.mu.Lock()
	defer bearer.mu.Unlock()
	bearer.retained = append(bearer.retained, handle)
	bearer.err = ErrCellularAccess
}

// Synthetic owner: no kernel ESP is installed by loopback unit tests. The
// registrar test explicitly supplies its fake installer through THIS owner,
// not through the Provider's process-global fallback.
func (bearer *fakeCellularBearer) BindProtectedSocket(ctx context.Context, transport string, binding imssec.SocketBinding, raw syscall.RawConn) error {
	bearer.mu.Lock()
	defer bearer.mu.Unlock()
	if raw == nil || binding.Validate(transport) != nil {
		return ErrCellularAccess
	}
	bearer.protected = append(bearer.protected, binding)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return bearer.protectErr
}

func (bearer *fakeCellularBearer) InstallIPSec(ctx context.Context, config imssec.Config) (imssec.Handle, error) {
	bearer.mu.Lock()
	installer := bearer.installer
	blocked := len(bearer.retained) != 0
	bearer.mu.Unlock()
	if installer == nil || blocked {
		return nil, ErrCellularAccess
	}
	return installer.Install(ctx, config)
}

type socketBinding struct{ network, address string }

func (bearer *fakeCellularBearer) BindSocket(ctx context.Context, network, address string, raw syscall.RawConn) error {
	bearer.mu.Lock()
	defer bearer.mu.Unlock()
	if raw == nil {
		return errors.New("no raw socket")
	}
	bearer.bindings = append(bearer.bindings, socketBinding{network, address})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return bearer.bindErr
}
func (*fakeCellularBearer) AcquireMediaRoute(context.Context, netip.Addr, netip.Addr) (vowifi.CellularMediaRoute, error) {
	return fakeCellularRoute{}, nil
}

type fakeCellularRoute struct{}

func (fakeCellularRoute) Close(context.Context) error { return nil }

func (bearer *fakeCellularBearer) Evidence() vowifi.CellularIMSEvidence {
	bearer.mu.Lock()
	defer bearer.mu.Unlock()
	result := bearer.evidence
	result.PCSCF = append([]string(nil), result.PCSCF...)
	return result
}

func (bearer *fakeCellularBearer) Verify(_ context.Context, identity vowifi.SIMIdentity) error {
	bearer.mu.Lock()
	defer bearer.mu.Unlock()
	bearer.verified = append(bearer.verified, identity)
	if bearer.failAt > 0 && len(bearer.verified) >= bearer.failAt {
		return ErrCellularAccess
	}
	return bearer.err
}

type changingCellularAKA struct {
	recordingAKA
	after func()
}

func (aka *changingCellularAKA) Authenticate(ctx context.Context, identity vowifi.SIMIdentity, challenge vowifi.AKAChallenge) (vowifi.AKAResult, error) {
	result, err := aka.recordingAKA.Authenticate(ctx, identity, challenge)
	if aka.after != nil {
		aka.after()
	}
	return result, err
}

func TestCellularIMSRechecksBeforeAndAfterAKA(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(strconv.FormatBool(after), func(t *testing.T) {
			request := testCellularRequest()
			bearer := request.Cellular.(*fakeCellularBearer)
			aka := &changingCellularAKA{recordingAKA: recordingAKA{result: vowifi.AKAResult{RES: []byte{1, 2, 3, 4}, CK: make([]byte, 16), IK: make([]byte, 16)}}}
			if after {
				aka.after = func() { bearer.err = ErrCellularAccess }
			} else {
				bearer.failAt = 2
			}
			installer := &fakeIPSecInstaller{}
			provider, err := NewProvider(aka, Config{IPSecInstaller: installer})
			if err != nil {
				t.Fatal(err)
			}
			access := imsAccessEvidence{LocalIPv4: bearer.evidence.LocalIPv4, LocalIPv6: bearer.evidence.LocalIPv6}
			connection := &registerRetransmitConn{writes: 1, response: []byte(strings.Join([]string{
				"SIP/2.0 401 Unauthorized", "Call-ID: cellular-card-check", "CSeq: 1 REGISTER",
				`WWW-Authenticate: Digest realm="ims.mnc001.mcc001.3gppnetwork.org", nonce="` + base64.StdEncoding.EncodeToString(make([]byte, 32)) + `", algorithm=AKAv1-MD5, qop="auth"`,
				"Security-Server: ipsec-3gpp;alg=hmac-sha-1-96;ealg=aes-cbc;prot=esp;mod=trans;spi-c=2001;spi-s=2002;port-c=5064;port-s=5065",
				"Content-Length: 0", "", ""}, "\r\n"))}
			session := &Session{provider: provider, request: request, access: access,
				identity:        identitySet{domain: "ims.mnc001.mcc001.3gppnetwork.org", private: "001010123456789@ims.mnc001.mcc001.3gppnetwork.org", public: "sip:001010123456789@ims.mnc001.mcc001.3gppnetwork.org"},
				initialEndpoint: pcscfEndpoint{host: "198.51.100.1", port: 5060},
				securityMode:    SecurityRequired, securityProposal: securityProposal{spiClient: 1001, spiServer: 1002, portClient: 5062, portServer: 5063},
				transport: "udp", conn: connection, callID: "cellular-card-check", cseq: 1}
			if _, err := session.register(context.Background(), 600); !errors.Is(err, ErrCellularAccess) {
				t.Fatalf("changed card accepted: %v", err)
			}
			wantAKA := 0
			if after {
				wantAKA = 1
			}
			if len(aka.challenges) != wantAKA || len(installer.installed()) != 0 || connection.writes != 2 {
				t.Fatal("AKA/IPsec/authenticated REGISTER occurred after ownership loss")
			}
		})
	}
}

func testCellularRequest() vowifi.IMSRequest {
	return vowifi.IMSRequest{DeviceID: "modem0",
		Identity: vowifi.SIMIdentity{ICCID: "8901000000000000000", IMSI: "001010123456789", HomeMCC: "001", HomeMNC: "01"},
		Cellular: &fakeCellularBearer{evidence: vowifi.CellularIMSEvidence{Established: true,
			LocalIPv4: "192.0.2.1", LocalIPv6: "2001:db8::1", PCSCF: []string{"198.51.100.1", "2001:db8::2"},
			Cell: vowifi.LTECell{MCC: "111", MNC: "22", Duplex: "FDD", TAC: 0x33c4, CellID: 0x76b4321}}}}
}

func TestCellularIMSAccessFailsClosedBeforeNetwork(t *testing.T) {
	for _, name := range []string{"both_accesses", "no_iccid", "no_imsi", "plain_sip", "optional_security", "disconnected", "verify_changed", "no_address", "wrong_family", "no_pcscf", "dns_pcscf", "multicast_pcscf", "missing_pcscf_family", "invalid_cell", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			request := testCellularRequest()
			bearer := request.Cellular.(*fakeCellularBearer)
			config := Config{SecurityMode: SecurityRequired}
			ctx := context.Background()
			switch name {
			case "both_accesses":
				request.Tunnel = evidenceTunnel{}
			case "no_iccid":
				request.Identity.ICCID = ""
			case "no_imsi":
				request.Identity.IMSI = ""
			case "plain_sip":
				config.SecurityMode = SecurityDisabled
			case "optional_security":
				config.SecurityMode = SecurityOptional
			case "disconnected":
				bearer.evidence.Established = false
			case "verify_changed":
				bearer.err = errors.New("private SIM identifier must not escape")
			case "no_address":
				bearer.evidence.LocalIPv4, bearer.evidence.LocalIPv6 = "", ""
			case "wrong_family":
				bearer.evidence.LocalIPv4 = "2001:db8::1"
			case "no_pcscf":
				bearer.evidence.PCSCF = nil
			case "dns_pcscf":
				bearer.evidence.PCSCF = []string{"ims.example.invalid"}
			case "multicast_pcscf":
				bearer.evidence.PCSCF = []string{"224.0.0.1"}
			case "missing_pcscf_family":
				bearer.evidence.LocalIPv6 = ""
			case "invalid_cell":
				bearer.evidence.Cell.MCC = ""
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			provider, err := NewProvider(&recordingAKA{}, config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Start(ctx, request); !errors.Is(err, ErrCellularAccess) {
				t.Fatalf("unsafe cellular request accepted: %v", err)
			}
		})
	}
}

func TestCellularIMSAccessPinsEveryBusinessOperation(t *testing.T) {
	request := testCellularRequest()
	bearer := request.Cellular.(*fakeCellularBearer)
	provider, err := NewProvider(&recordingAKA{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	access, err := provider.accessEvidence(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{provider: provider, request: request, access: access,
		initialEndpoint: pcscfEndpoint{host: "198.51.100.1", port: 5060}}
	for _, name := range []string{"sim_changed", "local_changed", "pcscf_changed"} {
		t.Run(name, func(t *testing.T) {
			original := bearer.evidence
			switch name {
			case "sim_changed":
				bearer.err = errors.New("removed SIM")
			case "local_changed":
				bearer.evidence.LocalIPv4 = "192.0.2.2"
			case "pcscf_changed":
				bearer.evidence.PCSCF = []string{"198.51.100.2"}
			}
			if _, err := session.DialCall(context.Background(), "+15551234567"); !errors.Is(err, ErrCellularAccess) {
				t.Fatalf("Dial: %v", err)
			}
			if _, err := session.AnswerCall(context.Background(), "call"); !errors.Is(err, ErrCellularAccess) {
				t.Fatalf("Answer: %v", err)
			}
			if _, err := session.SendSMS(context.Background(), vowifi.SMSSubmitRequest{}); !errors.Is(err, ErrCellularAccess) {
				t.Fatalf("SMS: %v", err)
			}
			if _, err := session.SendUSSI(context.Background(), vowifi.USSISubmitRequest{}); !errors.Is(err, ErrCellularAccess) {
				t.Fatalf("USSI: %v", err)
			}
			if _, err := session.register(context.Background(), 600); !errors.Is(err, ErrCellularAccess) {
				t.Fatalf("REGISTER: %v", err)
			}
			bearer.evidence, bearer.err = original, nil
		})
	}
	for _, identity := range bearer.verified {
		if identity.ICCID != request.Identity.ICCID || identity.IMSI != request.Identity.IMSI {
			t.Fatal("verification was not pinned to selected SIM")
		}
	}
}

func TestCellularIMSTransportCacheIsSeparateFromWiFi(t *testing.T) {
	provider := &Provider{transportCache: make(map[string]string)}
	cellular := testCellularRequest()
	wifi := cellular
	wifi.Cellular = nil
	provider.rememberRequestTransport(wifi, "udp")
	if got := provider.cachedRequestTransport(cellular); got != "" {
		t.Fatal("Wi-Fi transport leaked to cellular")
	}
	provider.rememberRequestTransport(cellular, "tcp")
	if provider.cachedRequestTransport(cellular) != "tcp" || provider.cachedRequestTransport(wifi) != "udp" {
		t.Fatal("access caches interfered")
	}
}

func TestSharedVoWinLTEPANIContract(t *testing.T) {
	// Read the repository contract explicitly; native test deployments copy it
	// beside the binary and pass that working directory (never silently skip).
	data, err := os.ReadFile("../../../../contracts/ims-lte-pani-v1.json")
	if err != nil {
		data, err = os.ReadFile("testdata/ims-lte-pani-v1.json")
	}
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Cases []struct {
			ID, Duplex, MCC, MNC, TAC, Cell string
			PANI                            *string
		}
	}
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	for _, tc := range contract.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			tac, tacErr := strconv.ParseUint(tc.TAC, 16, 16)
			eci, eciErr := strconv.ParseUint(tc.Cell, 16, 32)
			got, err := (vowifi.LTECell{MCC: tc.MCC, MNC: tc.MNC, Duplex: tc.Duplex, TAC: uint16(tac), CellID: uint32(eci)}).PAccessNetworkInfo()
			if tc.PANI == nil {
				if err == nil && tacErr == nil && eciErr == nil {
					t.Fatal("invalid cell accepted")
				}
			} else if err != nil || tacErr != nil || eciErr != nil || got != *tc.PANI {
				t.Fatalf("PANI=%q, err=%v", got, err)
			}
		})
	}
}
