package ims

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

type rtpTestNetwork struct {
	*fakeCellularBearer
	acquire func(context.Context, netip.Addr, netip.Addr) (vowifi.CellularMediaRoute, error)
}

func (network *rtpTestNetwork) AcquireMediaRoute(ctx context.Context, local, remote netip.Addr) (vowifi.CellularMediaRoute, error) {
	if network.acquire != nil {
		return network.acquire(ctx, local, remote)
	}
	return &rtpTestRoute{}, nil
}

type rtpTestRoute struct {
	mu      sync.Mutex
	err     error
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (route *rtpTestRoute) Close(ctx context.Context) error {
	route.mu.Lock()
	route.calls++
	err, entered, release := route.err, route.entered, route.release
	route.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func rtpTestSDP(address string, port int) []byte {
	return []byte(fmt.Sprintf("v=0\r\nc=IN IP4 %s\r\nm=audio %d RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n", address, port))
}

func rtpTestPeer(t *testing.T) *net.UDPConn {
	t.Helper()
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	return connection
}

func waitRTPEvent(t *testing.T, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic RTP event timed out")
	}
}

func TestCellularRTPBindsAndRouteFailureCannotSendPCM(t *testing.T) {
	network := &rtpTestNetwork{fakeCellularBearer: &fakeCellularBearer{}}
	network.acquire = func(context.Context, netip.Addr, netip.Addr) (vowifi.CellularMediaRoute, error) {
		return nil, errors.New("private carrier route")
	}
	media, err := newAccessRTPMedia(context.Background(), net.IPv4(127, 0, 0, 1), network, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	if len(network.bindings) != 1 || !addressIP(media.conn.LocalAddr()).Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatal("RTP socket bypassed bearer/source binding")
	}
	if err := media.configureRemote(rtpTestSDP("127.0.0.1", 19000)); !errors.Is(err, ErrCellularAccess) || strings.Contains(err.Error(), "private") || media.ready() {
		t.Fatalf("unproven target exposed: %v", err)
	}
	if err := media.WritePCM(make([]int16, 160)); err == nil {
		t.Fatal("PCM sent before route acquisition")
	}
	network.bindErr = errors.New("private socket owner")
	if connection, err := newAccessRTPMedia(context.Background(), net.IPv4(127, 0, 0, 1), network, nil); connection != nil || !errors.Is(err, ErrCellularAccess) || strings.Contains(err.Error(), "private") {
		t.Fatalf("failed binder exposed RTP: %v", err)
	}
}

func TestCellularRTPAtomicRemoteSwapDoesNotWaitForOldCleanup(t *testing.T) {
	left, right := rtpTestPeer(t), rtpTestPeer(t)
	first := &rtpTestRoute{entered: make(chan struct{}, 1), release: make(chan struct{})}
	second := &rtpTestRoute{}
	entered, release := make(chan struct{}), make(chan struct{})
	count := 0
	network := &rtpTestNetwork{fakeCellularBearer: &fakeCellularBearer{}}
	network.acquire = func(ctx context.Context, local, remote netip.Addr) (vowifi.CellularMediaRoute, error) {
		count++
		if local.String() != "127.0.0.1" || remote.String() != "127.0.0.1" {
			t.Error("incorrect route source/destination")
		}
		if count == 1 {
			return first, nil
		}
		close(entered)
		select {
		case <-release:
			return second, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	media, err := newAccessRTPMedia(context.Background(), net.IPv4(127, 0, 0, 1), network, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(first.release); media.Close(); media.DrainCellularRoutes(context.Background()) }()
	if err := media.configureRemote(rtpTestSDP("127.0.0.1", left.LocalAddr().(*net.UDPAddr).Port)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- media.configureRemote(rtpTestSDP("127.0.0.1", right.LocalAddr().(*net.UDPAddr).Port)) }()
	waitRTPEvent(t, entered)
	if err := media.WritePCM(make([]int16, 160)); err != nil {
		t.Fatal(err)
	}
	left.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := left.ReadFromUDP(make([]byte, 512)); err != nil {
		t.Fatal("old target lost before new route was ready", err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("remote swap waited for old route cleanup")
	}
	waitRTPEvent(t, first.entered)
	if err := media.WritePCM(make([]int16, 160)); err != nil {
		t.Fatal(err)
	}
	right.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := right.ReadFromUDP(make([]byte, 512)); err != nil {
		t.Fatal("new target not installed atomically", err)
	}
	closed := make(chan struct{})
	go func() { media.Close(); close(closed) }()
	waitRTPEvent(t, closed)
	if _, err := media.ReadPCM(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal("close did not stop media before route cleanup", err)
	}
}

func TestCellularRTPCancelledAcquireReclaimsLateHandle(t *testing.T) {
	entered := make(chan struct{})
	route := &rtpTestRoute{entered: make(chan struct{}, 1)}
	network := &rtpTestNetwork{fakeCellularBearer: &fakeCellularBearer{}}
	network.acquire = func(ctx context.Context, _, _ netip.Addr) (vowifi.CellularMediaRoute, error) {
		close(entered)
		<-ctx.Done()
		return route, nil
	}
	media, err := newAccessRTPMedia(context.Background(), net.IPv4(127, 0, 0, 1), network, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- media.configureRemote(rtpTestSDP("127.0.0.1", 19000)) }()
	waitRTPEvent(t, entered)
	media.Close()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not cancel pending route acquisition")
	}
	waitRTPEvent(t, route.entered)
	if err := media.DrainCellularRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if media.ready() {
		t.Fatal("late acquisition resurrected closed RTP")
	}
}

func TestCellularRTPFailedCleanupRetainsOwnerAndCanDrainAgain(t *testing.T) {
	route := &rtpTestRoute{err: errors.New("private cleanup failure"), entered: make(chan struct{}, 2)}
	network := &rtpTestNetwork{fakeCellularBearer: &fakeCellularBearer{}, acquire: func(context.Context, netip.Addr, netip.Addr) (vowifi.CellularMediaRoute, error) { return route, nil }}
	media, err := newAccessRTPMedia(context.Background(), net.IPv4(127, 0, 0, 1), network, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := media.configureRemote(rtpTestSDP("127.0.0.1", 19000)); err != nil {
		t.Fatal(err)
	}
	media.Close()
	waitRTPEvent(t, route.entered)
	if err := media.DrainCellularRoutes(context.Background()); !errors.Is(err, ErrCellularAccess) {
		t.Fatal("cleanup failure was hidden", err)
	}
	s, _ := newCallTestSession(t)
	ended := time.Now().Add(-time.Hour)
	s.calls["retained"] = &imsCall{public: vowifi.Call{ID: "retained", State: "ended", EndedAt: &ended}, media: media}
	if len(s.Calls()) != 1 || !media.hasCellularRouteCleanup() {
		t.Fatal("failed cleanup owner discarded by history eviction")
	}
	route.mu.Lock()
	route.err = nil
	route.mu.Unlock()
	media.Close()
	if err := media.DrainCellularRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if media.hasCellularRouteCleanup() {
		t.Fatal("successful retry retained route")
	}
}

func TestCellularRTPFailedRetirementQueueIsBounded(t *testing.T) {
	network := &rtpTestNetwork{fakeCellularBearer: &fakeCellularBearer{}}
	created := 0
	network.acquire = func(context.Context, netip.Addr, netip.Addr) (vowifi.CellularMediaRoute, error) {
		created++
		return &rtpTestRoute{err: errors.New("unconfirmed owned cleanup")}, nil
	}
	media, err := newAccessRTPMedia(context.Background(), net.IPv4(127, 0, 0, 1), network, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	refused := false
	for index := 0; index < maxRetiredCellularRoutes+2; index++ {
		if err := media.configureRemote(rtpTestSDP("127.0.0.1", 19000+index)); err != nil {
			if !errors.Is(err, ErrCellularAccess) {
				t.Fatal(err)
			}
			refused = true
			break
		}
	}
	if !refused || created > maxRetiredCellularRoutes {
		t.Fatal("failed cleanup grew without an admission bound", created)
	}
	media.Close()
	if err := media.DrainCellularRoutes(context.Background()); !errors.Is(err, ErrCellularAccess) {
		t.Fatal("unconfirmed handles forgotten", err)
	}
	media.cleanupMu.Lock()
	retained := len(media.retiredRoutes)
	media.cleanupMu.Unlock()
	if retained != created || retained > maxRetiredCellularRoutes {
		t.Fatal("failed route reference lost/overflowed", retained, created)
	}
}

func cellularRTPTestSession(t *testing.T, network *rtpTestNetwork) (*Session, *callTestConn) {
	t.Helper()
	s, conn := newCallTestSession(t)
	request := testCellularRequest()
	base := request.Cellular.(*fakeCellularBearer)
	base.evidence.LocalIPv4, base.evidence.LocalIPv6 = "127.0.0.1", ""
	base.evidence.PCSCF = []string{"127.0.0.1:15061"}
	network.fakeCellularBearer = base
	request.Cellular = network
	s.request = request
	s.access = imsAccessEvidence{LocalIPv4: "127.0.0.1"}
	s.initialEndpoint = s.endpoint
	return s, conn
}

func cellularDialogTestRequest(t *testing.T, call *imsCall, method string, sequence uint32) *sipRequest {
	t.Helper()
	packet, err := parseSIPPacket([]byte(strings.Join([]string{
		method + " " + headerURI(call.from) + " SIP/2.0",
		fmt.Sprintf("Via: SIP/2.0/UDP 127.0.0.1:15061;branch=z9hG4bK-dialog-%d-%s", sequence, method),
		"From: " + call.to, "To: " + call.from, "Call-ID: " + call.callID,
		fmt.Sprintf("CSeq: %d %s", sequence, method), "Content-Length: 0", "", "",
	}, "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	packet.Request.Body = []byte(callTestSDP)
	return packet.Request
}

func TestCellularIncomingInviteDoesNotBindBeforeAnswer(t *testing.T) {
	network := &rtpTestNetwork{}
	s, _ := cellularRTPTestSession(t, network)
	network.bindErr = errors.New("would block WDS")
	invite := incomingTestInvite(t, "cellular-deferred")
	invite.Body = []byte(callTestSDP)
	var replies []string
	s.handleCallRequest(invite, func(b []byte) error { replies = append(replies, string(b)); return nil })
	if len(s.Calls()) != 1 || len(network.bindings) != 0 {
		t.Fatal("receive loop tried to bind cellular media")
	}
	s.handleCallRequest(incomingTestRequest(t, invite, "CANCEL", 1, false), func(b []byte) error { replies = append(replies, string(b)); return nil })
	if len(replies) != 3 || !strings.HasPrefix(replies[2], "SIP/2.0 487 ") {
		t.Fatal("CANCEL not handled while media deferred", replies)
	}
}

func TestCellularPendingAnswerRouteDoesNotBlockCANCEL(t *testing.T) {
	entered := make(chan struct{})
	route := &rtpTestRoute{entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(route.release)
	network := &rtpTestNetwork{acquire: func(ctx context.Context, _, _ netip.Addr) (vowifi.CellularMediaRoute, error) {
		close(entered)
		<-ctx.Done()
		return route, nil
	}}
	s, _ := cellularRTPTestSession(t, network)
	invite := incomingTestInvite(t, "cellular-cancel-answer")
	invite.Body = []byte(callTestSDP)
	var replyMu sync.Mutex
	var replies []string
	respond := func(b []byte) error {
		replyMu.Lock()
		replies = append(replies, string(b))
		replyMu.Unlock()
		return nil
	}
	s.handleCallRequest(invite, respond)
	answer := make(chan error, 1)
	go func() { _, err := s.AnswerCall(context.Background(), invite.value("Call-ID")); answer <- err }()
	waitRTPEvent(t, entered)
	cancelled := make(chan struct{})
	go func() {
		s.handleCallRequest(incomingTestRequest(t, invite, "CANCEL", 1, false), respond)
		close(cancelled)
	}()
	waitRTPEvent(t, cancelled)
	select {
	case err := <-answer:
		if err == nil {
			t.Fatal("answer succeeded after CANCEL")
		}
	case <-time.After(time.Second):
		t.Fatal("answer did not observe call cancellation")
	}
	replyMu.Lock()
	defer replyMu.Unlock()
	if len(replies) != 3 || !strings.HasPrefix(replies[1], "SIP/2.0 200 ") || !strings.HasPrefix(replies[2], "SIP/2.0 487 ") {
		t.Fatal("answer raced past CANCEL", replies)
	}
	if s.Calls()[0].MediaReady || s.Calls()[0].State != "ended" {
		t.Fatal("CANCEL resurrected call")
	}
}

func TestCellularDialogMediaWorkerIsBoundedAndDoesNotBlockHangup(t *testing.T) {
	entered := make(chan struct{})
	network := &rtpTestNetwork{acquire: func(ctx context.Context, _, _ netip.Addr) (vowifi.CellularMediaRoute, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s, _ := cellularRTPTestSession(t, network)
	media, err := newAccessRTPMedia(context.Background(), net.IPv4(127, 0, 0, 1), network, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := &imsCall{callID: "worker", public: vowifi.Call{ID: "worker", State: "active"}, media: media,
		from: "<sip:subscriber@ims.example.test>;tag=local", to: "<tel:+15550000001>;tag=remote"}
	s.calls[call.callID] = call
	request := cellularDialogTestRequest(t, call, "UPDATE", 2)
	replies := make(chan string, 16)
	respond := func(b []byte) error { replies <- string(b); return nil }
	returned := make(chan struct{})
	go func() { s.handleDialogOffer(request, respond, call); close(returned) }()
	waitRTPEvent(t, returned)
	waitRTPEvent(t, entered)
	for index := 0; index < 8; index++ {
		s.handleDialogOffer(cellularDialogTestRequest(t, call, "UPDATE", uint32(index+3)), respond, call)
	}
	s.callMu.Lock()
	queued := len(call.mediaJobs)
	s.callMu.Unlock()
	if queued != 4 || len(replies) != 4 {
		t.Fatal("SDP receive work was unbounded", queued, len(replies))
	}
	finished := make(chan struct{})
	go func() { s.finishCall(call.callID, "ended", 0, ""); close(finished) }()
	waitRTPEvent(t, finished)
	if err := media.DrainCellularRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Calls()[0].MediaReady || s.Calls()[0].State != "ended" {
		t.Fatal("late media worker revived ended call")
	}
}

func TestCellularSDPPathsAcquireRoutes(t *testing.T) {
	for _, direction := range []string{"outgoing", "incoming-offer", "incoming-ack"} {
		t.Run(direction, func(t *testing.T) {
			var mu sync.Mutex
			acquired := 0
			network := &rtpTestNetwork{acquire: func(context.Context, netip.Addr, netip.Addr) (vowifi.CellularMediaRoute, error) {
				mu.Lock()
				acquired++
				mu.Unlock()
				return &rtpTestRoute{}, nil
			}}
			s, conn := cellularRTPTestSession(t, network)
			acceptCallTestControls(s, conn)
			var call *imsCall
			if direction == "outgoing" {
				public, err := s.DialCall(context.Background(), "+15550000002")
				if err != nil {
					t.Fatal(err)
				}
				s.callMu.Lock()
				call = s.calls[public.ID]
				s.callMu.Unlock()
				call.responses <- &sipResponse{StatusCode: 183, Headers: map[string][]string{"to": {"<tel:+15550000002>;tag=remote"}}, Body: []byte(callTestSDP)}
				waitCallTest(t, func() bool { return s.Calls()[0].State == "early_media" })
				call.responses <- &sipResponse{StatusCode: 200, Headers: map[string][]string{"to": {"<tel:+15550000002>;tag=remote"}}, Body: []byte(callTestSDP)}
				waitCallTest(t, func() bool { return s.Calls()[0].State == "active" })
			} else {
				invite := incomingTestInvite(t, "cellular-"+direction)
				if direction == "incoming-offer" {
					invite.Body = []byte(callTestSDP)
				}
				s.handleCallRequest(invite, func([]byte) error { return nil })
				if _, err := s.AnswerCall(context.Background(), invite.value("Call-ID")); err != nil {
					t.Fatal(err)
				}
				s.callMu.Lock()
				call = s.calls[invite.value("Call-ID")]
				s.callMu.Unlock()
				if direction == "incoming-ack" {
					ack := incomingTestRequest(t, invite, "ACK", 1, true)
					ack.Body = []byte(callTestSDP)
					s.handleCallACK(ack)
				}
				waitCallTest(t, func() bool { return s.Calls()[0].MediaReady })
			}
			mu.Lock()
			before := acquired
			mu.Unlock()
			if direction == "outgoing" && before != 2 || direction != "outgoing" && before != 1 {
				t.Fatal("SDP route path bypassed", before)
			}
			update := cellularDialogTestRequest(t, call, "UPDATE", 10)
			s.handleDialogOffer(update, func([]byte) error { return nil }, call)
			waitCallTest(t, func() bool { mu.Lock(); defer mu.Unlock(); return acquired == before+1 })
			s.finishCall(call.callID, "ended", 0, "")
			if err := call.media.DrainCellularRoutes(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
