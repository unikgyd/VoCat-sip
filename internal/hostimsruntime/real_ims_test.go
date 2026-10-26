package hostimsruntime

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"vocat/internal/vowifi"
	"vocat/internal/vowifi/ims"
	"vocat/internal/vowifi/imssec"
)

// These are synthetic ownership hooks, NOT actual ESP or kernel policies. All
// sockets are loopback-only; this test exercises the real SIP runtime consumer.
type registrarAccess struct {
	fakeAccess
	mu         sync.Mutex
	evidence   vowifi.CellularIMSEvidence
	bindings   []imssec.SocketBinding
	transports []string
	networks   []string
	akaCalls   atomic.Int32
	installs   atomic.Int32
	retained   atomic.Int32
	clientPort int
	serverPort int
	handle     *registrarHandle
}

type registrarHandle struct {
	trace  *callTrace
	closes atomic.Int32
}

func (h *registrarHandle) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.closes.Add(1)
	h.trace.add("ipsec")
	return nil
}

func (a *registrarAccess) Evidence() vowifi.CellularIMSEvidence {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.evidence
	out.PCSCF = append([]string(nil), out.PCSCF...)
	return out
}

func (a *registrarAccess) BindSocket(ctx context.Context, transport, address string, raw syscall.RawConn) error {
	if raw == nil {
		return errors.New("missing synthetic raw socket")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("refusing non-loopback test socket")
	}
	if err := raw.Control(func(uintptr) {}); err != nil {
		return err
	}
	a.mu.Lock()
	a.networks = append(a.networks, transport+":"+address)
	a.mu.Unlock()
	return ctx.Err()
}

func (a *registrarAccess) BindProtectedSocket(ctx context.Context, transport string, binding imssec.SocketBinding, raw syscall.RawConn) error {
	if raw == nil || binding.Validate(transport) != nil || !binding.Local.Addr().IsLoopback() || !binding.Remote.IsLoopback() {
		return errors.New("invalid synthetic required protection hook")
	}
	if err := raw.Control(func(uintptr) {}); err != nil {
		return err
	}
	a.mu.Lock()
	a.bindings, a.transports = append(a.bindings, binding), append(a.transports, transport)
	a.mu.Unlock()
	return ctx.Err()
}

func (a *registrarAccess) Authenticate(ctx context.Context, identity vowifi.SIMIdentity, challenge vowifi.AKAChallenge) (vowifi.AKAResult, error) {
	if err := a.Verify(ctx, identity); err != nil {
		return vowifi.AKAResult{}, err
	}
	for i := 0; i < 16; i++ {
		if challenge.RAND[i] != byte(i) || challenge.AUTN[i] != byte(i+16) {
			return vowifi.AKAResult{}, errors.New("SIP nonce was not mapped to the exact synthetic AKA challenge")
		}
	}
	a.akaCalls.Add(1)
	return vowifi.AKAResult{RES: []byte{1, 2, 3, 4, 5, 6, 7, 8}, CK: []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		IK: []byte{16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31}}, nil
}

func (a *registrarAccess) AuthenticateWithPreference(ctx context.Context, identity vowifi.SIMIdentity, challenge vowifi.AKAChallenge, preference string) (vowifi.AKAResult, error) {
	if preference != "" && preference != "isim_strict" {
		return vowifi.AKAResult{}, errors.New("unexpected synthetic AKA application")
	}
	return a.Authenticate(ctx, identity, challenge)
}

func (a *registrarAccess) InstallIPSec(ctx context.Context, config imssec.Config) (imssec.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := imssec.ValidateConfig(config); err != nil {
		return nil, err
	}
	if !config.LocalIP.Equal(net.ParseIP("127.0.0.1")) || !config.RemoteIP.Equal(net.ParseIP("127.0.0.2")) ||
		config.EncryptionAlgorithm != "aes-cbc" || config.IntegrityAlgorithm != "hmac-sha-1-96" || len(config.EncryptionKey) != 16 || len(config.IntegrityKey) != 20 {
		return nil, errors.New("wrong synthetic access security agreement")
	}
	a.mu.Lock()
	a.clientPort, a.serverPort = config.UEClientPort, config.UEServerPort
	a.mu.Unlock()
	a.installs.Add(1)
	return a.handle, nil
}

func (a *registrarAccess) RetainIPSec(imssec.Handle) { a.retained.Add(1) }

type forbiddenInstaller struct{ calls atomic.Int32 }

func (i *forbiddenInstaller) Install(context.Context, imssec.Config) (imssec.Handle, error) {
	i.calls.Add(1)
	return nil, errors.New("process-global installer must not protect the prepared bearer")
}

func readRegistrarPacket(listener *net.UDPConn) (string, map[string]string, *net.UDPAddr, error) {
	packet := make([]byte, 65535)
	n, remote, err := listener.ReadFromUDP(packet)
	if err != nil {
		return "", nil, nil, err
	}
	lines := strings.Split(string(packet[:n]), "\r\n")
	if len(lines) < 2 {
		return "", nil, nil, errors.New("malformed loopback SIP packet")
	}
	headers := map[string]string{}
	for _, line := range lines[1:] {
		if line == "" {
			break
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			return "", nil, nil, errors.New("malformed loopback SIP header")
		}
		headers[strings.ToLower(name)] = strings.TrimSpace(value)
	}
	return lines[0], headers, remote, nil
}

func registrarResponse(status int, headers map[string]string, extra ...string) []byte {
	reason := "OK"
	if status == 401 {
		reason = "Unauthorized"
	}
	lines := []string{fmt.Sprintf("SIP/2.0 %d %s", status, reason), "Call-ID: " + headers["call-id"], "CSeq: " + headers["cseq"],
		"Via: " + headers["via"], "From: " + headers["from"], "To: " + headers["to"] + ";tag=synthetic-registrar"}
	lines = append(lines, extra...)
	lines = append(lines, "Content-Length: 0", "", "")
	return []byte(strings.Join(lines, "\r\n"))
}

func verifyRegistrarDigest(value, nonce string, card cardProvisioning) error {
	if !strings.HasPrefix(value, "Digest ") {
		return errors.New("protected REGISTER has no Digest authorization")
	}
	directives := map[string]string{}
	for _, match := range regexp.MustCompile(`([A-Za-z0-9-]+)=(?:"([^"]*)"|([^,\s]+))`).FindAllStringSubmatch(strings.TrimPrefix(value, "Digest "), -1) {
		if _, exists := directives[match[1]]; exists {
			return errors.New("duplicate synthetic digest directive")
		}
		directives[match[1]] = match[2] + match[3]
	}
	if directives["username"] != card.private || directives["realm"] != card.domain || directives["uri"] != "sip:"+card.domain ||
		directives["nonce"] != nonce || directives["qop"] != "auth" || directives["algorithm"] != "AKAv1-MD5" || directives["cnonce"] == "" || directives["nc"] == "" {
		return errors.New("synthetic AKA digest used wrong provisioned identity, realm, URI or nonce")
	}
	md5hex := func(value string) string { sum := md5.Sum([]byte(value)); return hex.EncodeToString(sum[:]) }
	ha1 := md5hex(card.private + ":" + card.domain + ":" + string([]byte{1, 2, 3, 4, 5, 6, 7, 8}))
	ha2 := md5hex("REGISTER:sip:" + card.domain)
	want := md5hex(ha1 + ":" + nonce + ":" + directives["nc"] + ":" + directives["cnonce"] + ":auth:" + ha2)
	if directives["response"] != want {
		return errors.New("synthetic AKA RES did not authenticate the protected REGISTER")
	}
	return nil
}

func serveRuntimeRegistrar(initial, protected *net.UDPConn, card cardProvisioning, pani string, trace *callTrace) error {
	line, headers, remote, err := readRegistrarPacket(initial)
	if err != nil {
		return err
	}
	validate := func(line string, headers map[string]string) error {
		if line != "REGISTER sip:"+card.domain+" SIP/2.0" || !strings.Contains(headers["from"], card.public) ||
			!strings.Contains(headers["to"], card.public) || headers["p-access-network-info"] != pani ||
			!strings.Contains(headers["authorization"], `username="`+card.private+`"`) {
			return errors.New("REGISTER omitted card identities or same-bearer PANI")
		}
		return nil
	}
	if err := validate(line, headers); err != nil {
		return err
	}
	if headers["require"] != "sec-agree" || headers["security-client"] == "" || !strings.Contains(headers["authorization"], "integrity-protected=no") {
		return errors.New("initial REGISTER did not require the owned security agreement")
	}
	proposal := map[string]uint64{}
	for _, field := range strings.Split(strings.Split(headers["security-client"], ",")[0], ";")[1:] {
		name, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if ok && (name == "spi-c" || name == "spi-s" || name == "port-c" || name == "port-s") {
			proposal[name], err = strconv.ParseUint(value, 10, 32)
			if err != nil {
				return errors.New("invalid synthetic Security-Client")
			}
		}
	}
	clientSPI := uint64(0x70000001)
	for clientSPI == proposal["spi-c"] || clientSPI == proposal["spi-s"] {
		clientSPI++
	}
	serverSPI := clientSPI + 1
	for serverSPI == proposal["spi-c"] || serverSPI == proposal["spi-s"] {
		serverSPI++
	}
	securityServer := fmt.Sprintf("ipsec-3gpp;q=0.100;alg=hmac-sha-1-96;prot=esp;mod=trans;ealg=aes-cbc;spi-c=%d;spi-s=%d;port-c=%d;port-s=%d",
		clientSPI, serverSPI, initial.LocalAddr().(*net.UDPAddr).Port, protected.LocalAddr().(*net.UDPAddr).Port)
	nonceBytes := make([]byte, 32)
	for i := range nonceBytes {
		nonceBytes[i] = byte(i)
	}
	nonce := base64.StdEncoding.EncodeToString(nonceBytes)
	if _, err := initial.WriteToUDP(registrarResponse(401, headers,
		`WWW-Authenticate: Digest realm="`+card.domain+`", nonce="`+nonce+`", algorithm=AKAv1-MD5, qop="auth"`,
		"Security-Server: "+securityServer), remote); err != nil {
		return err
	}
	line, headers, remote, err = readRegistrarPacket(protected)
	if err != nil {
		return err
	}
	if err := validate(line, headers); err != nil {
		return err
	}
	if remote.Port != int(proposal["port-c"]) || headers["security-verify"] != securityServer ||
		!strings.Contains(headers["authorization"], "integrity-protected=yes") || !strings.Contains(headers["authorization"], "algorithm=AKAv1-MD5") ||
		!strings.Contains(headers["authorization"], `nonce="`+nonce+`"`) || !strings.Contains(headers["authorization"], `response="`) {
		return errors.New("protected REGISTER bypassed negotiated source, AKA or Security-Verify")
	}
	if err := verifyRegistrarDigest(headers["authorization"], nonce, card); err != nil {
		return err
	}
	// Protected UDP replies are delivered to the negotiated UE server socket,
	// not the UE client source port used for sending the REGISTER.
	reply := &net.UDPAddr{IP: remote.IP, Port: int(proposal["port-s"])}
	if _, err := protected.WriteToUDP(registrarResponse(200, headers, "P-Associated-URI: <"+card.public+">", "Contact: "+headers["contact"]+";expires=600"), reply); err != nil {
		return err
	}
	line, headers, remote, err = readRegistrarPacket(protected)
	if err != nil {
		return err
	}
	if err := validate(line, headers); err != nil {
		return err
	}
	if headers["expires"] != "0" || headers["security-verify"] != securityServer {
		return fmt.Errorf("Stop omitted protected deregistration (CSeq=%s expires=%s)", headers["cseq"], headers["expires"])
	}
	trace.add("unregister")
	_, err = protected.WriteToUDP(registrarResponse(200, headers), reply)
	return err
}

func TestRuntimeRealIMSProviderRegistersAndUnregistersThroughSameAccess(t *testing.T) {
	listen := func() *net.UDPConn {
		socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.2")})
		if err != nil {
			t.Fatal("loopback-only registrar unavailable", err)
		}
		if err := socket.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = socket.Close() })
		return socket
	}
	initial, protected := listen(), listen()
	card := testCard()
	trace := &callTrace{}
	a := &registrarAccess{handle: &registrarHandle{trace: trace}, evidence: vowifi.CellularIMSEvidence{Established: true,
		LocalIPv4: "127.0.0.1", PCSCF: []string{initial.LocalAddr().String()},
		Cell: vowifi.LTECell{MCC: "001", MNC: "01", Duplex: "FDD", TAC: 0x1234, CellID: 42}}}
	a.fakeAccess.trace = trace
	a.fakeAccess.verify = func(ctx context.Context, identity vowifi.SIMIdentity) error {
		if identity.ICCID != card.identity.ICCID || identity.IMSI != card.identity.IMSI {
			return errors.New("wrong synthetic SIM")
		}
		return ctx.Err()
	}
	a.fakeAccess.close = func(ctx context.Context, _ int32) error {
		if ctx.Err() != nil || a.handle.closes.Load() != 1 {
			return errors.New("access closed before SIP security handle")
		}
		a.mu.Lock()
		clientPort, serverPort := a.clientPort, a.serverPort
		a.mu.Unlock()
		for _, port := range []int{clientPort, serverPort} {
			conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
			if err != nil {
				return errors.New("access closed before the IMS UDP socket")
			}
			_ = conn.Close()
		}
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: serverPort})
		if err != nil {
			return errors.New("access closed before the IMS TCP listener")
		}
		return listener.Close()
	}
	pani, err := a.evidence.Cell.PAccessNetworkInfo()
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- serveRuntimeRegistrar(initial, protected, card, pani, trace) }()
	global := &forbiddenInstaller{}
	providerErrors := make(chan error, 1)
	r, err := newRuntimeWith(Config{DeviceID: "synthetic-loopback-line", IMS: ims.Config{Transport: "udp", TransactionTimeout: time.Second,
		SecurityMode: ims.SecurityDisabled, IPSecInstaller: global, HomeDomain: "forged.example.net", PrivateIdentity: "forged@forged.example.net", PublicIdentity: "sip:forged@forged.example.net"},
		Prepare: func(context.Context) (Access, error) { return a, nil }},
		func(aka vowifi.AKAProvider, config ims.Config) (vowifi.IMSProvider, error) {
			if config.IPSecInstaller != nil || config.SecurityMode != ims.SecurityRequired || config.HomeDomain != card.domain || config.PrivateIdentity != card.private || config.PublicIdentity != card.public {
				return nil, errors.New("runtime did not replace user identity/security with the prepared card")
			}
			// Inject an explicit forbidden global fallback into the REAL provider
			// to also prove that install is dispatched through this Access owner.
			config.IPSecInstaller = global
			realProvider, err := ims.NewProvider(aka, config)
			if err != nil {
				return nil, err
			}
			return providerFunc(func(ctx context.Context, request vowifi.IMSRequest) (vowifi.IMSSession, error) {
				session, err := realProvider.Start(ctx, request)
				providerErrors <- err
				return session, err
			}), nil
		}, func(Access) cardProvisioning { return card })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = r.Stop(ctx)
	})
	if err := r.Start(context.Background()); err != nil {
		providerErr := <-providerErrors
		select {
		case registrarErr := <-serverDone:
			t.Fatal("synthetic real IMS startup failed", err, "registrar:", registrarErr, "provider:", providerErr)
		default:
			t.Fatal("synthetic real IMS startup failed", err, "provider:", providerErr, "AKA/install:", a.akaCalls.Load(), a.installs.Load())
		}
	}
	if !r.Snapshot().Ready || r.Session() == nil || !r.Session().Evidence().SecurityVerified {
		t.Fatal("real IMS evidence was not published ready")
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal("synthetic real IMS shutdown failed", err)
	}
	if err := waitResult(t, serverDone); err != nil {
		t.Fatal("loopback registrar rejected the runtime", err)
	}
	if a.akaCalls.Load() != 1 || a.installs.Load() != 1 || global.calls.Load() != 0 || a.retained.Load() != 0 ||
		trace.value() != "unregister,ipsec,access" || a.closes.Load() != 1 || r.Snapshot().State != Stopped {
		t.Fatal("real SIP/owned-security lifecycle or cleanup order was not exercised", trace.value())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.bindings) != 3 {
		t.Fatal("required protected listener/client hooks were bypassed", len(a.bindings))
	}
	serverTCP, serverUDP, clientUDP := false, false, false
	for i, binding := range a.bindings {
		if binding.Role == imssec.ServerSocket && int(binding.Local.Port()) == a.serverPort {
			serverTCP = serverTCP || strings.HasPrefix(a.transports[i], "tcp")
			serverUDP = serverUDP || strings.HasPrefix(a.transports[i], "udp")
		}
		clientUDP = clientUDP || binding.Role == imssec.ClientSocket && int(binding.Local.Port()) == a.clientPort && strings.HasPrefix(a.transports[i], "udp")
	}
	if !serverTCP || !serverUDP || !clientUDP || len(a.networks) < 4 {
		t.Fatal("same-access network/protection hooks incomplete")
	}
}
