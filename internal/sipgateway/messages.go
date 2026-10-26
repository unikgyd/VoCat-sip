package sipgateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/emiago/sipgo/sip"
)

type MessageStatistics struct {
	Active    int    `json:"active"`
	Confirmed uint64 `json:"confirmed"`
	Accepted  uint64 `json:"accepted"`
	Rejected  uint64 `json:"rejected"`
	Unknown   uint64 `json:"unknown"`
}
type messageOperation struct {
	fingerprint [32]byte
	pending     bool
	code        int
	until       time.Time
}
type responseExpectation struct {
	source   string
	callID   string
	sequence uint32
	from     string
	to       string
	tag      string
}
type messageOutcome struct{ code int }

func (r *runtime) account(user string) (Account, bool) {
	for _, a := range r.config.Config.Accounts {
		if a.Username == user {
			return a, true
		}
	}
	return Account{}, false
}
func (r *runtime) identityDomain() string {
	if r.config.Config.IdentityDomain != "" {
		return r.config.Config.IdentityDomain
	}
	return r.config.Config.Realm
}

func (r *runtime) message(req *sip.Request, tx sip.ServerTransaction) {
	if !r.config.Config.MessagesEnabled && !r.config.Config.SMSEnabled {
		r.respond(req, tx, 405, "Method Not Allowed")
		return
	}
	principal, ok := r.authorizeBusiness(req, tx)
	if !ok {
		return
	}
	sender := principal.account
	user := sender.Username
	target := req.Recipient.User
	if len(target) > 180 || target == "" || req.To().Address.User != target || req.Recipient.Password != "" || len(req.Recipient.Headers) > 0 || req.To().Params.Has("tag") {
		r.respond(req, tx, 400, "Invalid Message Destination")
		return
	}
	receiver, exists := r.account(target)
	// A fixed peer is a separate authenticated carrier principal, not a local
	// extension or a route back to itself. Presentation identity never grants
	// access to the local registrar's message namespace.
	if principal.peer && (exists || r.config.Config.Peer == nil || !r.config.Config.Peer.Enabled || !r.config.Config.Peer.AllowSMSFromPeer) {
		r.respond(req, tx, 403, "Peer SMS Not Permitted")
		return
	}
	if exists && !r.config.Config.MessagesEnabled {
		r.respond(req, tx, 405, "Internal Messages Disabled")
		return
	}
	if exists && (!sender.AllowMessages || !receiver.AllowMessages) {
		r.respond(req, tx, 403, "Messages Not Permitted")
		return
	}
	if req.MaxForwards() == nil || len(req.GetHeaders("Max-Forwards")) != 1 {
		r.respond(req, tx, 400, "Missing Max-Forwards")
		return
	}
	if req.MaxForwards().Val() == 0 {
		r.respond(req, tx, 483, "Too Many Hops")
		return
	}
	contentTypes := req.GetHeaders("Content-Type")
	mediaType := ""
	parameters := map[string]string{}
	var err error
	if len(contentTypes) == 1 {
		mediaType, parameters, err = mime.ParseMediaType(contentTypes[0].Value())
	}
	charset := strings.ToLower(parameters["charset"])
	encodings := req.GetHeaders("Content-Encoding")
	if len(contentTypes) != 1 || err != nil || mediaType != "text/plain" || len(encodings) > 1 || len(encodings) == 1 && !strings.EqualFold(strings.TrimSpace(encodings[0].Value()), "identity") || charset != "" && charset != "utf-8" && charset != "us-ascii" {
		r.respond(req, tx, 415, "Unsupported Media Type", sip.NewHeader("Accept", "text/plain; charset=utf-8"))
		return
	}
	body := req.Body()
	if charset == "us-ascii" {
		for _, b := range body {
			if b > 127 {
				r.respond(req, tx, 400, "Invalid ASCII Body")
				return
			}
		}
	}
	if len(body) > 4096 {
		r.respond(req, tx, 413, "Message Too Large")
		return
	}
	if len(req.GetHeaders("Content-Length")) != 1 || req.ContentLength() == nil || int(*req.ContentLength()) != len(body) || len(body) == 0 || !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		r.respond(req, tx, 400, "Invalid Message Body")
		return
	}
	if !exists {
		if !extensionPattern.MatchString(target) && !internationalNumberPattern.MatchString(target) && !isSMSReplyNamespace(target) && !plainNumberPattern.MatchString(target) && !prefixedInternationalSMS.MatchString(target) {
			r.respond(req, tx, 400, "Invalid Message Destination")
			return
		}
		r.carrierSMS(req, tx, sender, body)
		return
	}
	// Retry identity is independent of the Via branch and the client's LAN/VPN
	// domain. A changed payload or recipient under that identity is a conflict.
	id := user + "\x00" + req.CallID().Value() + "\x00" + strconv.FormatUint(uint64(req.CSeq().SeqNo), 10)
	fingerprint := sha256.Sum256(append([]byte(target+"\x00text/plain\x00"), body...))
	r.mu.Lock()
	r.pruneLocked(time.Now())
	if previous := r.messageOperations[id]; previous != nil {
		code := previous.code
		if previous.fingerprint != fingerprint {
			code = 400
		} else if previous.pending {
			code = 503
		}
		r.mu.Unlock()
		r.messageResult(req, tx, code)
		return
	}
	if r.closed.Load() || len(r.messageOperations) >= 2048 || len(r.messageTargets) >= 32 || r.messageTargets[target] {
		r.mu.Unlock()
		r.messageResult(req, tx, 503)
		return
	}
	// A peer with several aliases at the same observed socket gets one copy.
	// Keep the most recently refreshed Contact for that socket.
	bySource := map[string]registration{}
	for _, entry := range r.registrations[target] {
		old, found := bySource[entry.source]
		if !found || entry.order > old.order || entry.order == old.order && entry.contact.Address.String() < old.contact.Address.String() {
			bySource[entry.source] = entry
		}
	}
	if len(bySource) == 0 {
		r.mu.Unlock()
		r.respond(req, tx, 480, "Extension Unavailable")
		return
	}
	targets := make([]registration, 0, len(bySource))
	for _, entry := range bySource {
		targets = append(targets, entry)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].source < targets[j].source })
	r.messageOperations[id] = &messageOperation{fingerprint: fingerprint, pending: true, until: time.Now().Add(5 * time.Minute)}
	r.messageTargets[target] = true
	r.work.Add(1)
	r.mu.Unlock()
	defer r.work.Done()
	ctx, cancel := context.WithTimeout(r.ctx, r.messageTimeout)
	defer cancel()
	results := make(chan messageOutcome, len(targets))
	var jobs sync.WaitGroup
	for _, entry := range targets {
		jobs.Add(1)
		go func(entry registration) {
			defer jobs.Done()
			results <- r.deliverMessage(ctx, req, sender, receiver, localDeliveryTarget(entry), body)
		}(entry)
	}
	jobs.Wait()
	close(results)
	code := aggregateMessages(results)
	r.mu.Lock()
	operation := r.messageOperations[id]
	operation.pending = false
	operation.code = code
	delete(r.messageTargets, target)
	switch code {
	case 200:
		r.messageStatistics.Confirmed++
	case 202:
		r.messageStatistics.Accepted++
	case 504:
		r.messageStatistics.Unknown++
	default:
		r.messageStatistics.Rejected++
	}
	r.mu.Unlock()
	r.messageResult(req, tx, code)
}

func aggregateMessages(results <-chan messageOutcome) int {
	seen := map[int]bool{}
	for result := range results {
		seen[result.code] = true
	}
	for _, code := range []int{200, 202, 504, 403, 415, 513, 503, 502, 480} {
		if seen[code] {
			return code
		}
	}
	return 502
}
func (r *runtime) messageResult(req *sip.Request, tx sip.ServerTransaction, code int) {
	reason := "Message Delivery Failed"
	headers := []sip.Header{}
	switch code {
	case 200:
		reason = "OK"
	case 202:
		reason = "Accepted"
	case 400:
		reason = "Conflicting Message Retry"
	case 403:
		reason = "Message Rejected"
	case 415:
		reason = "Unsupported Media Type"
		headers = append(headers, sip.NewHeader("Accept", "text/plain; charset=utf-8"))
	case 480:
		reason = "Extension Unavailable"
	case 503:
		reason = "Message Service Busy"
		headers = append(headers, sip.NewHeader("Retry-After", "1"))
	case 502:
		reason = "Message Delivery Failed"
	case 504:
		reason = "Delivery Unknown"
		headers = append(headers, sip.NewHeader("X-VoCat-Delivery", "unknown"))
	case 513:
		reason = "Message Too Large"
	}
	r.respond(req, tx, code, reason, headers...)
}

func (r *runtime) deliverMessage(ctx context.Context, incoming *sip.Request, sender, receiver Account, entry deliveryTarget, body []byte, guards ...func() bool) messageOutcome {
	request := sip.NewRequest(sip.MESSAGE, *entry.contact.Address.Clone())
	request.SetTransport("UDP")
	request.SetDestination(entry.source)
	from := &sip.FromHeader{DisplayName: sender.DisplayName, Address: sip.Uri{Scheme: "sip", User: sender.Username, Host: r.identityDomain()}}
	from.Params.Add("tag", randomToken())
	to := &sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: receiver.Username, Host: r.identityDomain()}}
	callID := sip.CallIDHeader(randomToken())
	cseq := &sip.CSeqHeader{SeqNo: 1, MethodName: sip.MESSAGE}
	local := r.conn.LocalAddr().(*net.UDPAddr)
	host := local.IP.String()
	if local.IP.IsUnspecified() {
		// UDP connect discovers the kernel-selected source address without
		// transmitting data. Keep signaling on the original listener socket.
		remote, err := net.ResolveUDPAddr("udp", entry.source)
		if err != nil {
			return messageOutcome{503}
		}
		probe, err := net.DialUDP("udp", nil, remote)
		if err != nil {
			return messageOutcome{503}
		}
		host = probe.LocalAddr().(*net.UDPAddr).IP.String()
		probe.Close()
	}
	branch := "z9hG4bK-" + randomToken()
	via := &sip.ViaHeader{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: host, Port: local.Port}
	via.Params.Add("branch", branch)
	via.Params.Add("rport", "")
	forward := sip.MaxForwardsHeader(incoming.MaxForwards().Val() - 1)
	request.AppendHeader(via)
	request.AppendHeader(from)
	request.AppendHeader(to)
	request.AppendHeader(&callID)
	request.AppendHeader(cseq)
	request.AppendHeader(&forward)
	request.AppendHeader(sip.NewHeader("Content-Type", "text/plain; charset=utf-8"))
	request.AppendHeader(sip.NewHeader("User-Agent", "VoCat"))
	request.SetBody(append([]byte(nil), body...))
	for attempt := 0; ; attempt++ {
		var response *sip.Response
		var failed int
		if len(guards) > 0 {
			// Only durable MT payloads use the guarded wire. sipgo's client
			// transaction retransmits directly and cannot await a fresh source
			// read without holding its response FSM lock.
			response, failed = r.guardedMessageResponse(ctx, request, entry.source, guards...)
		} else {
			response, failed = r.messageResponse(ctx, request, entry.source)
		}
		if response == nil {
			return messageOutcome{failed}
		}
		code := response.StatusCode
		if entry.peer && attempt == 0 && (code == 401 || code == 407) {
			header, value, ok := r.peerAuthorization(request, response, entry.source)
			if !ok || ctx.Err() != nil {
				return messageOutcome{403}
			}
			// A challenge is a new wire transaction, not another logical SMS.
			// Keep the caller's original deadline and the persisted Unknown.
			request = request.Clone()
			request.Via().Params.Add("branch", "z9hG4bK-"+randomToken())
			request.CSeq().SeqNo++
			request.RemoveHeader("Authorization")
			request.RemoveHeader("Proxy-Authorization")
			request.AppendHeader(sip.NewHeader(header, value))
			continue
		}
		switch {
		case code == 200:
			return messageOutcome{200}
		case code >= 200 && code < 300:
			return messageOutcome{202}
		case code >= 300 && code < 400 || code == 401 || code == 403 || code == 407 || code == 603:
			return messageOutcome{403}
		case code == 415:
			return messageOutcome{415}
		case code == 413 || code == 513:
			return messageOutcome{513}
		case code == 404 || code == 408 || code == 410 || code == 480 || code == 486:
			return messageOutcome{480}
		case code == 503:
			return messageOutcome{503}
		default:
			return messageOutcome{502}
		}
	}
}

// One transaction owns one exact response identity. Retire it before a peer
// challenge retry is admitted, so old branches cannot complete the new send.
func (r *runtime) messageResponse(ctx context.Context, request *sip.Request, source string) (*sip.Response, int) {
	// RFC 3428 applies to authenticated retries too, not only the first packet.
	if len(request.String()) > 1300 {
		return nil, 513
	}
	if ctx.Err() != nil {
		return nil, 504
	}
	branch := request.Via().Params.GetOr("branch", "")
	expect := responseExpectation{source: source, callID: request.CallID().Value(), sequence: request.CSeq().SeqNo, from: request.From().Address.String(), to: request.To().Address.String(), tag: request.From().Params.GetOr("tag", "")}
	r.mu.Lock()
	r.pendingResponses[branch] = expect
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pendingResponses, branch); r.mu.Unlock() }()
	transaction, err := r.client.TransactionRequest(ctx, request)
	if err != nil {
		return nil, 504
	}
	defer transaction.Terminate()
	for {
		select {
		case response := <-transaction.Responses():
			if response == nil {
				return nil, 504
			}
			if response.IsProvisional() {
				continue
			}
			return response, 0
		case <-transaction.Done():
			return nil, 504
		case <-ctx.Done():
			return nil, 504
		}
	}
}

func randomToken() string {
	var value [16]byte
	_, _ = rand.Read(value[:])
	return hex.EncodeToString(value[:])
}

// Validate replies BEFORE sipgo's transaction state machine sees them. Branch
// matching alone is not enough: a foreign source or mismatched Call-ID must
// not terminate a delivery transaction and turn an unconfirmed send green.
func (r *runtime) responseFilter() sip.TransportReadFilter {
	parser := registrarParser()
	return func(props sip.TransportReadProps, data []byte) ([]byte, error) {
		if bytes.HasPrefix(data, []byte("CANCEL ")) || bytes.HasPrefix(data, []byte("ACK ")) || bytes.HasPrefix(data, []byte("BYE ")) {
			parsed, err := parser.ParseSIP(data)
			if err != nil {
				return nil, nil
			}
			request, ok := parsed.(*sip.Request)
			if !ok {
				return nil, nil
			}
			request.SetSource(props.RemoteAddr.String())
			request.SetTransport("UDP")
			if request.Method == sip.CANCEL {
				r.cancelCall(request, props.RemoteAddr.String())
				return nil, nil
			}
			if request.Method == sip.BYE {
				// Validate before sipgo allocates a transaction: a forged BYE
				// must not poison the real peer's transaction/cache by branch.
				if r.checkCallDialog(request, props.RemoteAddr.String(), false, false) == nil {
					r.statelessCallResponse(request, props.RemoteAddr.String(), 481)
					return nil, nil
				}
				return data, nil
			}
			if r.callDialog(request, props.RemoteAddr.String(), true) != nil {
				return nil, nil
			}
			return data, nil
		}
		if !bytes.HasPrefix(data, []byte("SIP/2.0")) {
			return data, nil
		}
		parsed, err := parser.ParseSIP(data)
		if err != nil {
			return nil, nil
		}
		response, ok := parsed.(*sip.Response)
		if !ok {
			return nil, nil
		}
		for _, name := range []string{"Via", "From", "To", "Call-ID", "CSeq", "Content-Length"} {
			if len(response.GetHeaders(name)) != 1 {
				return nil, nil
			}
		}
		if _, ok := response.GetHeaders("From")[0].(*sip.FromHeader); !ok {
			return nil, nil
		}
		if _, ok := response.GetHeaders("To")[0].(*sip.ToHeader); !ok {
			return nil, nil
		}
		if response.Via() == nil || response.From() == nil || response.To() == nil || response.CallID() == nil || response.CSeq() == nil || response.ContentLength() == nil || int(*response.ContentLength()) != len(response.Body()) || response.StatusCode < 100 || response.StatusCode > 699 {
			return nil, nil
		}
		branch, _ := response.Via().Params.Get("branch")
		if found, _ := r.routeCallResponse(response, props.RemoteAddr.String()); found {
			return nil, nil
		}
		if r.routeSMSPeerProbeResponse(response, props.RemoteAddr.String()) {
			return nil, nil
		}
		if r.routeMTMessageResponse(response, props.RemoteAddr.String()) {
			return nil, nil
		}
		r.mu.Lock()
		expect, found := r.pendingResponses[branch]
		r.mu.Unlock()
		if !found || props.RemoteAddr.String() != expect.source || response.CallID().Value() != expect.callID || response.CSeq().MethodName != sip.MESSAGE || response.CSeq().SeqNo != expect.sequence || response.From().Address.String() != expect.from || response.To().Address.String() != expect.to || response.From().Params.GetOr("tag", "") != expect.tag {
			return nil, nil
		}
		return data, nil
	}
}
