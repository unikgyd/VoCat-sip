package sipgateway

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/emiago/sipgo/sip"
)

// Call client transactions are owned here rather than by DialogClientSession:
// cancellation must stop INVITE retransmission immediately and retain only a
// bounded late-response path, without waiting for the peer during local exit.
type callWire struct {
	request   *sip.Request
	expect    responseExpectation
	responses chan *sip.Response
	toTag     string
}

func wireKey(branch string, method sip.RequestMethod) string { return branch + "|" + string(method) }
func (r *runtime) trackCallRequest(request *sip.Request, source string) (*callWire, error) {
	branch, _ := request.Via().Params.Get("branch")
	wire := &callWire{request: request, responses: make(chan *sip.Response, 32), expect: responseExpectation{source: source, callID: request.CallID().Value(), sequence: request.CSeq().SeqNo, from: request.From().Address.String(), to: request.To().Address.String(), tag: request.From().Params.GetOr("tag", "")}}
	wire.toTag = request.To().Params.GetOr("tag", "")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() || len(r.callWires) >= 512 {
		return nil, errors.New("call transaction capacity reached")
	}
	key := wireKey(branch, request.Method)
	if r.callWires[key] != nil {
		return nil, errors.New("duplicate call transaction")
	}
	r.callWires[key] = wire
	return wire, nil
}
func (r *runtime) removeCallWire(wire *callWire) {
	branch, _ := wire.request.Via().Params.Get("branch")
	r.mu.Lock()
	delete(r.callWires, wireKey(branch, wire.request.Method))
	r.mu.Unlock()
}
func (r *runtime) sendCallPacket(request *sip.Request, source string) error {
	remote, err := net.ResolveUDPAddr("udp", source)
	if err != nil {
		return err
	}
	data := []byte(request.String())
	if len(data) > 1300 {
		return errors.New("SIP request exceeds safe UDP size")
	}
	_, err = r.conn.WriteTo(data, remote)
	return err
}
func (r *runtime) callVia(source string, branch string) (*sip.ViaHeader, error) {
	local := r.conn.LocalAddr().(*net.UDPAddr)
	ip := local.IP
	if ip.IsUnspecified() {
		var err error
		ip, err = sourceLocalIP(source)
		if err != nil {
			return nil, err
		}
	}
	if branch == "" {
		branch = "z9hG4bK-" + randomToken()
	}
	via := &sip.ViaHeader{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: ip.String(), Port: local.Port}
	via.Params.Add("branch", branch)
	via.Params.Add("rport", "")
	return via, nil
}
func (r *runtime) callContact(source string) (*sip.ContactHeader, error) {
	via, err := r.callVia(source, "")
	if err != nil {
		return nil, err
	}
	return &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "gateway", Host: via.Host, Port: via.Port}}, nil
}
func (r *runtime) buildCallRequest(method sip.RequestMethod, recipient sip.Uri, source string, from *sip.FromHeader, to *sip.ToHeader, id string, cseq uint32, branch string, body []byte) (*sip.Request, error) {
	via, err := r.callVia(source, branch)
	if err != nil {
		return nil, err
	}
	request := sip.NewRequest(method, *recipient.Clone())
	request.SetTransport("UDP")
	request.SetDestination(source)
	request.AppendHeader(via)
	request.AppendHeader(sip.HeaderClone(from))
	request.AppendHeader(sip.HeaderClone(to))
	callID := sip.CallIDHeader(id)
	request.AppendHeader(&callID)
	request.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: method})
	maxForwards := sip.MaxForwardsHeader(70)
	request.AppendHeader(&maxForwards)
	if method == sip.INVITE {
		contact, err := r.callContact(source)
		if err != nil {
			return nil, err
		}
		request.AppendHeader(contact)
		request.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	request.SetBody(append([]byte(nil), body...))
	return request, nil
}
func (r *runtime) callDo(ctx context.Context, request *sip.Request, source string) error {
	for attempt := 0; attempt < 2; attempt++ {
		response, err := r.callDoResponse(ctx, request, source)
		if err != nil {
			return err
		}
		if response.StatusCode < 300 {
			return nil
		}
		// ACK/CANCEL are never blindly retried with credentials. A peer BYE
		// may retry once, under the SAME cleanup context and total deadline.
		if attempt == 0 && request.Method == sip.BYE && ctx.Err() == nil {
			if name, value, ok := r.peerAuthorization(request, response, source); ok && request.CSeq().SeqNo+1 < 1<<31 {
				retry, err := r.buildCallRequest(sip.BYE, request.Recipient, source, request.From(), request.To(), request.CallID().Value(), request.CSeq().SeqNo+1, "", request.Body())
				if err != nil {
					return err
				}
				retry.AppendHeader(sip.NewHeader(name, value))
				request = retry
				continue
			}
		}
		return errors.New("call request rejected")
	}
	return errors.New("call request authentication exhausted")
}
func (r *runtime) callDoResponse(ctx context.Context, request *sip.Request, source string) (*sip.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wire, err := r.trackCallRequest(request, source)
	if err != nil {
		return nil, err
	}
	defer r.removeCallWire(wire)
	delay := 500 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	if err = r.sendCallPacket(request, source); err != nil {
		return nil, err
	}
	for {
		select {
		case response := <-wire.responses:
			if response.StatusCode >= 200 {
				return response, nil
			}
		case <-timer.C:
			if err = r.sendCallPacket(request, source); err != nil {
				return nil, err
			}
			delay = min(2*delay, 4*time.Second)
			timer.Reset(delay)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (r *runtime) routeCallResponse(response *sip.Response, source string) (bool, bool) {
	branch, _ := response.Via().Params.Get("branch")
	r.mu.Lock()
	wire := r.callWires[wireKey(branch, response.CSeq().MethodName)]
	r.mu.Unlock()
	if wire == nil {
		return false, false
	}
	expect := wire.expect
	if wire.toTag != "" && response.To().Params.GetOr("tag", "") != wire.toTag {
		return true, false
	}
	if source != expect.source || response.CallID().Value() != expect.callID || response.CSeq().SeqNo != expect.sequence || response.From().Address.String() != expect.from || response.To().Address.String() != expect.to || response.From().Params.GetOr("tag", "") != expect.tag {
		return true, false
	}
	select {
	case wire.responses <- response:
	default:
	}
	return true, true
}
