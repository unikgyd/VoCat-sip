package ims

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"vocat/internal/vowifi"
	"vocat/internal/vowifi/imssec"
)

// Intentionally exposes the network but NOT its security capability.
type networkOnlyCellular struct {
	vowifi.CellularIMSNetwork
	base *fakeCellularBearer
}

func (b networkOnlyCellular) Evidence() vowifi.CellularIMSEvidence { return b.base.Evidence() }
func (b networkOnlyCellular) Verify(ctx context.Context, id vowifi.SIMIdentity) error {
	return b.base.Verify(ctx, id)
}

func TestCellularSecurityOwnerRequiredBeforeAnyNetwork(t *testing.T) {
	request := testCellularRequest()
	base := request.Cellular.(*fakeCellularBearer)
	request.Cellular = networkOnlyCellular{CellularIMSNetwork: base, base: base}
	installer, aka := &fakeIPSecInstaller{}, &recordingAKA{}
	provider, err := NewProvider(aka, Config{IPSecInstaller: installer})
	if err != nil {
		t.Fatal(err)
	}
	if session, err := provider.Start(context.Background(), request); session != nil || !errors.Is(err, ErrCellularAccess) {
		t.Fatal("network-only bearer used a global security installer", err)
	}
	if len(base.bindings) != 0 || len(base.verified) != 0 || len(aka.challenges) != 0 || len(installer.installed()) != 0 {
		t.Fatal("SIP/AKA/global installation attempted without owned security")
	}
}

func TestCellularProtectedSocketsFailBeforeListenOrConnect(t *testing.T) {
	for _, operation := range []string{"tcp-listen", "udp-listen", "tcp-dial", "udp-dial"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			base := &fakeCellularBearer{protectErr: errors.New("private SIM and driver detail")}
			port, err := availableProtectedPort(net.IPv4(127, 0, 0, 1), 0)
			if err != nil {
				t.Fatal(err)
			}
			role := imssec.ServerSocket
			if strings.HasSuffix(operation, "dial") {
				role = imssec.ClientSocket
			}
			binding, _ := cellularSocketBinding(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), port, role)
			var result net.Conn
			switch operation {
			case "tcp-listen":
				listener, listenErr := listenProtectedAccessTCP(ctx, net.IPv4(127, 0, 0, 1), port, base, &binding)
				err = listenErr
				if listener != nil {
					listener.Close()
					t.Fatal("unprotected listener exposed")
				}
			case "udp-listen":
				connection, listenErr := listenProtectedAccessUDP(ctx, net.IPv4(127, 0, 0, 1), port, base, &binding)
				err = listenErr
				if connection != nil {
					connection.Close()
					t.Fatal("unprotected UDP exposed")
				}
			default:
				result, err = dialProtectedSIPOnAccess(ctx, strings.TrimSuffix(operation, "-dial"), "127.0.0.1", port, "127.0.0.1:9", true, base, &binding)
				if result != nil {
					result.Close()
					t.Fatal("unprotected connection exposed")
				}
			}
			if !errors.Is(err, ErrCellularAccess) || strings.Contains(err.Error(), "private") || len(base.protected) != 1 || len(base.bindings) != 1 {
				t.Fatalf("protection bypass/disclosure: %v", err)
			}
			if base.protected[0] != binding {
				t.Fatal("protected socket used wrong endpoint/role")
			}
			// A rejected Control hook must not leave the UDP or TCP port bound.
			probe, err := listenAccessTCP(ctx, net.IPv4(127, 0, 0, 1), port, nil)
			if err != nil {
				t.Fatal("failed protected listener leaked its port", err)
			}
			probe.Close()
			packet, err := listenAccessUDP(ctx, net.IPv4(127, 0, 0, 1), port, nil)
			if err != nil {
				t.Fatal("failed protected packet socket leaked its port", err)
			}
			packet.Close()
		})
	}
}

func TestProtectedHookComposesBeforeExistingDialControl(t *testing.T) {
	packet, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer packet.Close()
	raw, _ := packet.SyscallConn()
	binding, _ := cellularSocketBinding(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 2), 22000, imssec.ClientSocket)
	for _, deny := range []bool{false, true} {
		base := &fakeCellularBearer{}
		if deny {
			base.protectErr = errors.New("denied")
		}
		existing := 0
		dialer := net.Dialer{Control: func(string, string, syscall.RawConn) error {
			existing++
			if len(base.bindings) != 1 || len(base.protected) != 1 {
				t.Fatal("existing hook ran before bearer/socket policies")
			}
			return nil
		}}
		bindProtectedCellularDialer(&dialer, base, &binding)
		err := dialer.ControlContext(context.Background(), "udp4", "127.0.0.2:22001", raw)
		if deny && (!errors.Is(err, ErrCellularAccess) || existing != 0) || !deny && (err != nil || existing != 1) {
			t.Fatal("control chaining ignored required protection", err)
		}
	}
	// An accidental nil bearer must not discard a requested socket policy.
	dialer := net.Dialer{}
	bindProtectedCellularDialer(&dialer, nil, &binding)
	if err := dialer.ControlContext(context.Background(), "udp4", "127.0.0.2:22001", raw); !errors.Is(err, ErrCellularAccess) {
		t.Fatal("nil bearer downgraded protected socket")
	}
}

type callbackIPSecInstaller func(context.Context, IPSecSAConfig) (IPSecSAHandle, error)

func (f callbackIPSecInstaller) Install(ctx context.Context, c IPSecSAConfig) (IPSecSAHandle, error) {
	return f(ctx, c)
}

type retryIPSecHandle struct {
	calls int
	fail  bool
}

func (h *retryIPSecHandle) Close(ctx context.Context) error {
	h.calls++
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("unbounded security cleanup")
	}
	if h.fail {
		return errors.New("private kernel owner detail")
	}
	return nil
}

func TestCellularInstallNeverUsesGlobalBackendAndRetainsLateHandle(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel_after_ack", "error_with_handle"} {
		t.Run(outcome, func(t *testing.T) {
			global := &fakeIPSecInstaller{}
			base := &fakeCellularBearer{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			handle := &retryIPSecHandle{}
			ownerCalls := 0
			base.installer = callbackIPSecInstaller(func(context.Context, IPSecSAConfig) (IPSecSAHandle, error) {
				ownerCalls++
				switch outcome {
				case "failure":
					return nil, errors.New("private kernel detail")
				case "cancel_after_ack":
					cancel()
				case "error_with_handle":
					return handle, errors.New("private rollback detail")
				}
				return handle, nil
			})
			session := &Session{provider: &Provider{installer: global}, request: vowifi.IMSRequest{Cellular: base}}
			got, err := session.installAccessIPSec(ctx, IPSecSAConfig{})
			if outcome == "success" {
				if got != handle || err != nil {
					t.Fatal("same-bearer installer ignored", err)
				}
			} else if got != nil || !errors.Is(err, ErrCellularAccess) || strings.Contains(err.Error(), "private") {
				t.Fatal("owner failure leaked or downgraded", err)
			}
			if ownerCalls != 1 || len(global.installed()) != 0 {
				t.Fatal("owned installation fell back to a global backend")
			}
			if outcome == "cancel_after_ack" || outcome == "error_with_handle" {
				if session.ipsecHandle != handle || session.closeAccessIPSec() != nil || session.ipsecHandle != nil || handle.calls != 1 {
					t.Fatal("late acknowledged security handle was lost")
				}
			}
		})
	}
}

func TestCellularSecurityCleanupFailureIsRedactedRetainedAndRetried(t *testing.T) {
	handle := &retryIPSecHandle{fail: true}
	session := &Session{request: testCellularRequest(), ipsecHandle: handle}
	if err := session.closeAccessIPSec(); !errors.Is(err, ErrCellularAccess) || strings.Contains(err.Error(), "private") || session.ipsecHandle != handle {
		t.Fatal("failed security cleanup lost ownership or leaked details", err)
	}
	handle.fail = false
	if err := session.closeAccessIPSec(); err != nil || session.ipsecHandle != nil || handle.calls != 2 {
		t.Fatal("security cleanup not retried", err)
	}
	if err := session.closeAccessIPSec(); err != nil || handle.calls != 2 {
		t.Fatal("already deleted security objects targeted again")
	}
}

func TestClosedCellularSessionRetriesSecurityAfterExpiredSIPDeadline(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	done := make(chan struct{})
	close(done)
	handle := &retryIPSecHandle{fail: true}
	session := &Session{conn: client, request: testCellularRequest(), ipsecHandle: handle,
		refreshCancel: func() {}, refreshDone: done}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.Close(ctx); !errors.Is(err, ErrCellularAccess) || session.ipsecHandle != handle {
		t.Fatal("expired SIP context hid or discarded security cleanup failure", err)
	}
	handle.fail = false
	if err := session.Close(ctx); err != nil || handle.calls != 2 || session.ipsecHandle != nil {
		t.Fatal("second Close did not finish the retained security cleanup", err)
	}
}

func TestCellularFailedStartTransfersUnresolvedSecurityToBearerOwner(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		peer, err := listener.AcceptTCP()
		if err != nil {
			done <- err
			return
		}
		defer peer.Close()
		peer.SetDeadline(time.Now().Add(3 * time.Second))
		packet, err := readSIPPacket(bufio.NewReader(peer))
		if err != nil || packet.Request == nil {
			done <- errors.New("synthetic REGISTER not parsed")
			return
		}
		req := packet.Request
		reply := []byte(strings.Join([]string{"SIP/2.0 401 Unauthorized",
			"Via: " + req.value("Via"), "From: " + req.value("From"), "To: " + req.value("To") + ";tag=lab",
			"Call-ID: " + req.value("Call-ID"), "CSeq: " + req.value("CSeq"),
			`WWW-Authenticate: Digest realm="ims.mnc001.mcc001.3gppnetwork.org", nonce="` + base64.StdEncoding.EncodeToString(make([]byte, 32)) + `", algorithm=AKAv1-MD5, qop="auth"`,
			"Security-Server: ipsec-3gpp;alg=hmac-sha-1-96;ealg=aes-cbc;prot=esp;mod=trans;spi-c=2001;spi-s=2002;port-c=5064;port-s=5065",
			"Content-Length: 0", "", ""}, "\r\n"))
		_, err = peer.Write(reply)
		done <- err
	}()
	request := testCellularRequest()
	base := request.Cellular.(*fakeCellularBearer)
	base.evidence.LocalIPv4, base.evidence.LocalIPv6 = "127.0.0.1", ""
	base.evidence.PCSCF = []string{listener.Addr().String()}
	handle := &retryIPSecHandle{fail: true}
	base.installer = callbackIPSecInstaller(func(context.Context, IPSecSAConfig) (IPSecSAHandle, error) {
		return handle, errors.New("private unknown install cleanup")
	})
	global := &fakeIPSecInstaller{}
	provider, err := NewProvider(&recordingAKA{result: vowifi.AKAResult{RES: []byte{1, 2, 3, 4}, CK: make([]byte, 16), IK: make([]byte, 16)}},
		Config{Transport: "tcp", TransactionTimeout: time.Second, IPSecInstaller: global})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if session, err := provider.Start(ctx, request); session != nil || !errors.Is(err, ErrIPSecInstall) || strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe failed startup result", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(base.retained) != 1 || base.retained[0] != handle || handle.calls != 1 || len(global.installed()) != 0 {
		t.Fatal("failed startup discarded its unresolved security handle")
	}
	if _, err := base.InstallIPSec(ctx, IPSecSAConfig{}); !errors.Is(err, ErrCellularAccess) {
		t.Fatal("same owner retried while security cleanup was unresolved")
	}
	bindings := len(base.bindings)
	if next, err := provider.Start(ctx, request); next != nil || !errors.Is(err, ErrCellularAccess) || len(base.bindings) != bindings {
		t.Fatal("failed owner issued another REGISTER instead of staying fenced", err)
	}
	// The live bearer owner can reclaim the transferred handle independently
	// of the failed SIP session, which the caller never received.
	handle.fail = false
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
	defer cleanupCancel()
	if err := base.retained[0].Close(cleanupCtx); err != nil || handle.calls != 2 {
		t.Fatal("retained owner handle cannot be reclaimed", err)
	}
}
