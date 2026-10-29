package sipgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Capability flags describe the implemented and enabled feature, not carrier
// registration or media readiness. SIM SMS requires separate explicit permissions.
type Capabilities struct {
	Registration    bool `json:"registration"`
	Calls           bool `json:"calls"`
	Messages        bool `json:"messages"`
	CarrierBridge   bool `json:"carrier_bridge"`
	CarrierOutgoing bool `json:"carrier_outgoing"`
	CarrierIncoming bool `json:"carrier_incoming"`
	CarrierSMS      bool `json:"carrier_sms"`
	FixedPeer       bool `json:"fixed_peer"`
}

// Configured is not a REGISTER, reachability probe or media acceptance result.
type PeerStatus struct {
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint,omitempty"`
	State    string `json:"state"`
}
type Binding struct {
	Username  string    `json:"username"`
	Source    string    `json:"source"`
	ExpiresAt time.Time `json:"expires_at"`
}
type Status struct {
	Running      bool              `json:"running"`
	Listen       string            `json:"listen"`
	Error        string            `json:"error,omitempty"`
	Bindings     []Binding         `json:"bindings"`
	Capabilities Capabilities      `json:"capabilities"`
	Messages     MessageStatistics `json:"message_statistics"`
	Calls        []CallStatus      `json:"calls"`
	SMS          SMSStatistics     `json:"sms_statistics"`
	Peer         PeerStatus        `json:"peer"`
}
type registration struct {
	contact *sip.ContactHeader
	source  string // observed UDP source, NEVER a Contact-controlled destination
	until   time.Time
	order   uint64
}
type replay struct {
	count uint64
	until time.Time
}
type sequence struct {
	number uint32
	until  time.Time
}
type runtime struct {
	config               stored
	key                  [32]byte
	opaque               string
	ua                   *sipgo.UserAgent
	client               *sipgo.Client
	ctx                  context.Context
	cancel               context.CancelFunc
	work                 sync.WaitGroup
	conn                 net.PacketConn
	done                 chan struct{}
	closed               atomic.Bool
	mu                   sync.Mutex
	replays              map[string]replay
	sequences            map[string]sequence
	registrations        map[string]map[string]registration
	serveError           string
	messageOperations    map[string]*messageOperation
	messageTargets       map[string]bool
	pendingResponses     map[string]responseExpectation
	mtMessageWires       map[string]*mtMessageWire
	smsPeerProbe         *smsPeerProbe
	smsPeerProbeUntil    time.Time
	smsPeerProbeReady    bool
	smsPeerWorker        bool
	messageStatistics    MessageStatistics
	messageTimeout       time.Duration
	registrationSequence uint64
	calls                map[string]*sipCall
	callLegs             map[string]*callLeg
	callBusy             map[string]*sipCall
	callWires            map[string]*callWire
	branchSlots          int // includes not-yet-started and late-answer-draining forks
	callRingTimeout      time.Duration
	callAckTimeout       time.Duration
	callLateTimeout      time.Duration
	callProbeInterval    time.Duration
	callProbeTimeout     time.Duration
	carrier              CarrierBackend
	journal              *callJournal
	carrierBusy          map[string]*carrierLeg
	sms                  *smsService
}

func start(config stored, options ...runtimeOptions) (*runtime, error) {
	r := &runtime{config: config, done: make(chan struct{}), replays: map[string]replay{}, sequences: map[string]sequence{}, registrations: map[string]map[string]registration{}}
	if len(options) > 0 {
		r.carrier, r.journal, r.sms = options[0].carrier, options[0].journal, options[0].sms
	}
	if config.Config.CarrierEnabled && (r.carrier == nil || r.journal == nil) {
		return nil, errors.New("carrier calls require an IMS backend and durable operation store")
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	ready := false
	defer func() {
		if !ready {
			r.cancel()
		}
	}()
	r.messageOperations = map[string]*messageOperation{}
	r.messageTargets = map[string]bool{}
	r.pendingResponses = map[string]responseExpectation{}
	r.mtMessageWires = map[string]*mtMessageWire{}
	r.messageTimeout = 5 * time.Second
	r.calls = map[string]*sipCall{}
	r.callLegs = map[string]*callLeg{}
	r.callBusy = map[string]*sipCall{}
	r.carrierBusy = map[string]*carrierLeg{}
	r.callWires = map[string]*callWire{}
	r.callRingTimeout = 30 * time.Second
	r.callAckTimeout = 32 * time.Second
	r.callLateTimeout = 32 * time.Second
	r.callProbeInterval = 30 * time.Second
	r.callProbeTimeout = 32 * time.Second
	if _, err := rand.Read(r.key[:]); err != nil {
		return nil, err
	}
	var opaque [16]byte
	if _, err := rand.Read(opaque[:]); err != nil {
		return nil, err
	}
	r.opaque = hex.EncodeToString(opaque[:])
	// Upstream may log raw protocol data at debug level. Never pass the app's
	// diagnostic logger, which users can export, into the transport library.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("VoCat"), sipgo.WithUserAgentParser(registrarParser()), sipgo.WithUserAgentTransactionLayerOptions(sip.WithTransactionLayerLogger(quiet)), sipgo.WithUserAgentTransportLayerOptions(sip.WithTransportLayerLogger(quiet), sip.WithTransportLayerReadFilter(r.responseFilter())))
	if err != nil {
		return nil, err
	}
	r.ua = ua
	srv, err := sipgo.NewServer(ua, sipgo.WithServerLogger(quiet))
	if err != nil {
		ua.Close()
		return nil, err
	}
	network := "udp"
	if listenAddr, addrErr := netip.ParseAddrPort(config.Config.Listen); addrErr == nil {
		if listenAddr.Addr().Is4() {
			network = "udp4"
		} else if listenAddr.Addr().Is6() {
			network = "udp6"
		}
	}
	conn, err := net.ListenPacket(network, config.Config.Listen)
	if err != nil {
		ua.Close()
		return nil, err
	}
	if err = validatePeerListener(config.Config.Peer, conn.LocalAddr().String()); err != nil {
		conn.Close()
		ua.Close()
		return nil, err
	}
	listenerReady := make(chan struct{})
	filter := &guardedPacketConn{PacketConn: conn, rates: map[netip.Addr]rate{}, peers: map[string]struct{}{}, ready: listenerReady}
	for _, cidr := range config.Config.AllowedCIDRs {
		prefix, _ := netip.ParsePrefix(cidr)
		filter.allowed = append(filter.allowed, prefix)
	}
	r.conn = filter
	r.client, err = sipgo.NewClient(ua, sipgo.WithClientLogger(quiet), sipgo.WithClientConnectionAddr(conn.LocalAddr().String()))
	if err != nil {
		conn.Close()
		ua.Close()
		r.cancel()
		return nil, err
	}
	srv.OnRegister(r.register)
	srv.OnMessage(r.message)
	srv.OnInvite(r.invite)
	srv.OnBye(r.bye)
	srv.OnInfo(r.info)
	// ACK has no response, even when no dialog exists. Replying 405 to an ACK
	// can create a response loop with a standards-compliant peer.
	srv.OnAck(func(_ *sip.Request, _ sip.ServerTransaction) {})
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		if !r.validRequest(req, tx) {
			return
		}
		if req.To().Params.Has("tag") && !r.validOptionsDialog(req, req.Source(), true) {
			r.respond(req, tx, 481, "Unknown Dialog")
			return
		}
		r.respond(req, tx, 200, "OK")
	})
	srv.OnNoRoute(func(req *sip.Request, tx sip.ServerTransaction) {
		if !r.validRequest(req, tx) {
			return
		}
		r.respond(req, tx, 405, "Method Not Allowed")
	})
	go func() {
		defer close(r.done)
		_ = srv.ServeUDP(filter)
		if !r.closed.Load() {
			r.mu.Lock()
			r.serveError = "SIP listener stopped; save settings to retry"
			r.mu.Unlock()
		}
	}()
	// sipgo installs this shared UDP socket in its connection pool before its
	// first ReadFrom. Do not publish an outbound-capable runtime sooner: a
	// peer needs no REGISTER and can otherwise race pool setup on its first
	// MESSAGE, causing a second bind and an ambiguous durable delivery.
	select {
	case <-listenerReady:
	case <-r.done:
		r.close()
		return nil, errors.New("SIP listener failed to start")
	}
	ready = true
	return r, nil
}

func (r *runtime) close() {
	if r.closed.Swap(true) {
		return
	}
	r.closeCalls()
	// Synchronize with work.Add under mu before waiting; no new job can be
	// admitted after closed was set. Cancel network work before closing its UA.
	r.mu.Lock()
	r.cancel()
	r.mu.Unlock()
	_ = r.conn.Close()
	// Carrier-call termination is asynchronous and does not deregister IMS.
	<-r.done
	_ = r.ua.Close()
	r.work.Wait()
}
func (r *runtime) status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(time.Now())
	result := Status{Running: !r.closed.Load() && r.serveError == "", Listen: r.conn.LocalAddr().String(), Error: r.serveError, Bindings: []Binding{}, Capabilities: Capabilities{Registration: true, Messages: r.config.Config.MessagesEnabled, Calls: r.config.Config.CallsEnabled}, Messages: r.messageStatistics, Calls: []CallStatus{}}
	result.Capabilities.CarrierOutgoing = r.config.Config.CarrierEnabled && r.carrier != nil && r.config.Config.CallsEnabled
	_, incoming := r.carrier.(CarrierIncomingBackend)
	result.Capabilities.CarrierIncoming = result.Capabilities.CarrierOutgoing && incoming
	result.Capabilities.CarrierBridge = result.Capabilities.CarrierOutgoing && result.Capabilities.CarrierIncoming
	result.Capabilities.CarrierSMS = r.config.Config.SMSEnabled && r.sms != nil && r.sms.backend != nil
	result.Peer = PeerStatus{State: "disabled"}
	if peer := r.config.Config.Peer; peer != nil && peer.Enabled {
		result.Capabilities.FixedPeer = true
		result.Peer = PeerStatus{Enabled: true, Endpoint: peer.Endpoint, State: "configured"}
		if !result.Running {
			result.Peer.State = "unavailable"
		}
	}
	for _, call := range r.calls {
		result.Calls = append(result.Calls, call.snapshot())
	}
	result.Messages.Active = len(r.messageTargets)
	for user, contacts := range r.registrations {
		for _, entry := range contacts {
			result.Bindings = append(result.Bindings, Binding{Username: user, Source: entry.source, ExpiresAt: entry.until})
		}
	}
	sort.Slice(result.Bindings, func(i, j int) bool {
		a, b := result.Bindings[i], result.Bindings[j]
		if a.Username == b.Username {
			return a.Source < b.Source
		}
		return a.Username < b.Username
	})
	return result
}
func (r *runtime) pruneLocked(now time.Time) {
	for k, v := range r.replays {
		if !v.until.After(now) {
			delete(r.replays, k)
		}
	}
	for k, v := range r.sequences {
		if !v.until.After(now) {
			delete(r.sequences, k)
		}
	}
	for key, operation := range r.messageOperations {
		if !operation.pending && !operation.until.After(now) {
			delete(r.messageOperations, key)
		}
	}
	for user, contacts := range r.registrations {
		for k, v := range contacts {
			if !v.until.After(now) {
				delete(contacts, k)
			}
		}
		if len(contacts) == 0 {
			delete(r.registrations, user)
		}
	}
}
func (r *runtime) respond(req *sip.Request, tx sip.ServerTransaction, code int, reason string, headers ...sip.Header) {
	response := sip.NewResponseFromRequest(req, code, reason, nil)
	// Never let a forged Via sent-by/received/rport redirect our response.
	response.SetDestination(req.Source())
	allow := "REGISTER, OPTIONS"
	if r.config.Config.MessagesEnabled || r.config.Config.SMSEnabled {
		allow += ", MESSAGE"
	}
	if r.config.Config.CallsEnabled {
		allow += ", INVITE, ACK, CANCEL, BYE"
	}
	response.AppendHeader(sip.NewHeader("Allow", allow))
	for _, header := range headers {
		response.AppendHeader(header)
	}
	_ = tx.Respond(response)
}
func (r *runtime) validRequest(req *sip.Request, tx sip.ServerTransaction) bool {
	for _, name := range []string{"From", "To", "Call-ID", "CSeq"} {
		if len(req.GetHeaders(name)) != 1 {
			r.respond(req, tx, 400, "Bad Request")
			return false
		}
	}
	// Do not invoke From() on a deliberately generic malformed header: the
	// library's lazy fallback would normalize away duplicate tag parameters.
	if _, ok := req.GetHeaders("From")[0].(*sip.FromHeader); !ok {
		r.respond(req, tx, 400, "Bad Request")
		return false
	}
	if _, ok := req.GetHeaders("To")[0].(*sip.ToHeader); !ok {
		r.respond(req, tx, 400, "Bad Request")
		return false
	}
	if req.From() == nil || req.To() == nil || req.Via() == nil || req.CallID() == nil || req.CSeq() == nil || len(req.CallID().Value()) > 256 || req.CSeq().MethodName != req.Method || req.CSeq().SeqNo >= 1<<31 || req.Recipient.Scheme != "sip" {
		r.respond(req, tx, 400, "Bad Request")
		return false
	}
	if required := req.GetHeaders("Require"); len(required) > 0 {
		unsupported := make([]string, 0, len(required))
		for _, header := range required {
			unsupported = append(unsupported, header.Value())
		}
		r.respond(req, tx, 420, "Bad Extension", sip.NewHeader("Unsupported", strings.Join(unsupported, ", ")))
		return false
	}
	return true
}
func (r *runtime) authorize(req *sip.Request, tx sip.ServerTransaction) (string, bool) {
	if !r.validRequest(req, tx) {
		return "", false
	}
	host, _, err := net.SplitHostPort(req.Source())
	if err != nil {
		return "", false
	}
	headers := req.GetHeaders("Authorization")
	if len(headers) > 1 {
		r.respond(req, tx, 400, "Bad Request")
		return "", false
	}
	value := ""
	if len(headers) == 1 {
		value = headers[0].Value()
	}
	user, ok := r.authenticate(string(req.Method), req.Recipient.String(), host, value)
	if !ok {
		r.respond(req, tx, 401, "Unauthorized", sip.NewHeader("WWW-Authenticate", r.challenge(host, false)))
		return "", false
	}
	if req.From().Address.User != user || req.Method == sip.REGISTER && req.To().Address.User != user {
		r.respond(req, tx, 403, "Forbidden")
		return "", false
	}
	return user, true
}

func expiry(value string) (int, error) {
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0, errors.New("invalid expires")
	}
	n, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, err
	}
	if n > 3600 {
		n = 3600
	}
	return int(n), nil
}
func (r *runtime) register(req *sip.Request, tx sip.ServerTransaction) {
	if r.isPeerSource(req.Source()) {
		if r.validRequest(req, tx) {
			r.respond(req, tx, 403, "Fixed Peer Does Not Register")
		}
		return
	}
	user, ok := r.authorize(req, tx)
	if !ok {
		return
	}
	if len(req.Body()) != 0 {
		r.respond(req, tx, 400, "Bad Request")
		return
	}
	global := 3600
	expiresHeaders := req.GetHeaders("Expires")
	if len(expiresHeaders) > 1 {
		r.respond(req, tx, 400, "Bad Request")
		return
	}
	if len(expiresHeaders) == 1 {
		var err error
		global, err = expiry(expiresHeaders[0].Value())
		if err != nil {
			r.respond(req, tx, 400, "Bad Request")
			return
		}
	}
	contacts := req.GetHeaders("Contact")
	if len(contacts) > 8 {
		r.respond(req, tx, 403, "Too Many Contacts")
		return
	}
	type update struct {
		key     string
		contact *sip.ContactHeader
		seconds int
	}
	updates := []update{}
	wildcard := false
	keys := map[string]bool{}
	for _, header := range contacts {
		contact, valid := header.(*sip.ContactHeader)
		if !valid {
			r.respond(req, tx, 400, "Bad Request")
			return
		}
		if contact.Address.Wildcard {
			if len(contacts) != 1 || len(expiresHeaders) != 1 || global != 0 || len(contact.Params) != 0 {
				r.respond(req, tx, 400, "Bad Request")
				return
			}
			wildcard = true
			continue
		}
		uri := contact.Address
		if uri.Scheme != "sip" || uri.Host == "" || uri.Password != "" || len(uri.Headers) > 0 || len(contact.Value()) > 2048 {
			r.respond(req, tx, 400, "Bad Request")
			return
		}
		seconds := global
		seenExpiry := false
		for _, param := range contact.Params {
			if strings.EqualFold(param.K, "expires") {
				if seenExpiry {
					r.respond(req, tx, 400, "Bad Request")
					return
				}
				seenExpiry = true
				var err error
				seconds, err = expiry(param.V)
				if err != nil {
					r.respond(req, tx, 400, "Bad Request")
					return
				}
			}
		}
		if seconds > 0 && seconds < 60 {
			r.respond(req, tx, 423, "Interval Too Brief", sip.NewHeader("Min-Expires", "60"))
			return
		}
		key := uri.String()
		if keys[key] {
			r.respond(req, tx, 400, "Bad Request")
			return
		}
		keys[key] = true
		updates = append(updates, update{key: key, contact: contact.Clone(), seconds: seconds})
	}
	now := time.Now()
	r.mu.Lock()
	r.pruneLocked(now)
	// Preserve a sequence tombstone even after a binding was removed so an old
	// authenticated REGISTER cannot resurrect it out of order.
	sequenceKey := user + "\x00" + req.CallID().Value()
	if len(contacts) > 0 {
		if old, exists := r.sequences[sequenceKey]; exists && req.CSeq().SeqNo <= old.number {
			r.mu.Unlock()
			r.respond(req, tx, 500, "CSeq Out of Order", sip.NewHeader("Retry-After", "1"))
			return
		}
		if _, exists := r.sequences[sequenceKey]; !exists && len(r.sequences) >= 1024 {
			r.mu.Unlock()
			r.respond(req, tx, 503, "Capacity Reached")
			return
		}
		staged := map[string]registration{}
		if !wildcard {
			for key, entry := range r.registrations[user] {
				staged[key] = entry
			}
		}
		for _, u := range updates {
			if u.seconds == 0 {
				delete(staged, u.key)
			} else {
				r.registrationSequence++
				staged[u.key] = registration{contact: u.contact, source: req.Source(), until: now.Add(time.Duration(u.seconds) * time.Second), order: r.registrationSequence}
			}
		}
		if len(staged) > 8 {
			r.mu.Unlock()
			r.respond(req, tx, 403, "Too Many Contacts")
			return
		}
		r.registrations[user] = staged
		r.sequences[sequenceKey] = sequence{number: req.CSeq().SeqNo, until: now.Add(time.Hour)}
	}
	headerKeys := make([]string, 0, len(r.registrations[user]))
	for key := range r.registrations[user] {
		headerKeys = append(headerKeys, key)
	}
	sort.Strings(headerKeys)
	headers := []sip.Header{}
	for _, key := range headerKeys {
		entry := r.registrations[user][key]
		contact := entry.contact.Clone()
		contact.Params.Remove("expires")
		contact.Params.Add("expires", strconv.Itoa(int((time.Until(entry.until)+time.Second-1)/time.Second)))
		headers = append(headers, contact)
	}
	r.mu.Unlock()
	r.respond(req, tx, 200, "OK", headers...)
}

// Filter before SIP parsing/transaction allocation. This is a trusted LAN/VPN
// service, not an Internet SBC. Lifetime peer cap also bounds sipgo's UDP pool.
type rate struct {
	start time.Time
	count int
}
type guardedPacketConn struct {
	net.PacketConn
	ready     chan struct{}
	readyOnce sync.Once
	allowed   []netip.Prefix
	rates     map[netip.Addr]rate
	peers     map[string]struct{}
}

func (c *guardedPacketConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	if c.ready != nil {
		c.readyOnce.Do(func() { close(c.ready) })
	}
	for {
		n, peer, err := c.PacketConn.ReadFrom(buffer)
		if err != nil {
			return n, peer, err
		}
		if n == 0 || n > 16*1024 {
			continue
		}
		udp, ok := peer.(*net.UDPAddr)
		if !ok {
			continue
		}
		ip := udp.AddrPort().Addr().Unmap()
		allowed := false
		for _, prefix := range c.allowed {
			if prefix.Contains(ip) {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		now := time.Now()
		rate, exists := c.rates[ip]
		if !exists && len(c.rates) >= 2048 {
			for k, v := range c.rates {
				if now.Sub(v.start) > 10*time.Second {
					delete(c.rates, k)
				}
			}
			if len(c.rates) >= 2048 {
				continue
			}
		}
		if now.Sub(rate.start) > 10*time.Second {
			rate.start = now
			rate.count = 0
		}
		rate.count++
		c.rates[ip] = rate
		if rate.count > 60 {
			continue
		}
		if _, exists := c.peers[peer.String()]; !exists {
			if len(c.peers) >= 4096 {
				continue
			}
			c.peers[peer.String()] = struct{}{}
		}
		return n, peer, nil
	}
}
