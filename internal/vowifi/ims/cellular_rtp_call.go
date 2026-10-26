package ims

import (
	"context"
	"io"
	"strings"
	"sync"
)

// callMu must be held. The context belongs to the call, not a UI request.
func (session *Session) callMediaContextLocked(call *imsCall) context.Context {
	if call.mediaContext == nil {
		call.mediaContext, call.mediaCancel = context.WithCancel(session.callContext())
	}
	return call.mediaContext
}

func (session *Session) configureCallMedia(call *imsCall, body []byte) error {
	return session.configureCallMediaWithCommit(call, nil, body, nil)
}

func (session *Session) configureCallMediaWithCommit(call *imsCall, ctx context.Context, body []byte, commit func(func() error) error) error {
	session.callMu.Lock()
	media := call.media
	if ctx == nil {
		ctx = session.callMediaContextLocked(call)
	}
	ended := call.terminated || call.public.EndedAt != nil
	session.callMu.Unlock()
	if ended || media == nil {
		return io.EOF
	}
	return media.configureRemoteWithCommit(ctx, body, commit)
}

func (session *Session) prepareCellularAnswerMedia(parent, lifetime context.Context, call *imsCall) error {
	ctx, cancel := context.WithTimeout(parent, cellularRTPTimeout)
	stop := context.AfterFunc(lifetime, cancel)
	defer func() { stop(); cancel() }()
	session.callMu.Lock()
	existing := call.media
	ended := call.terminated || call.public.EndedAt != nil
	session.callMu.Unlock()
	if ended {
		return io.EOF
	}
	if existing != nil {
		return nil
	}
	media, err := newAccessRTPMedia(ctx, session.localMediaIP(), session.request.Cellular, openNativeAMR)
	if err != nil {
		return ErrCellularAccess
	}
	session.callMu.Lock()
	ended = call.terminated || call.public.EndedAt != nil || ctx.Err() != nil
	if !ended {
		call.media = media
	}
	session.callMu.Unlock()
	if ended {
		_ = media.Close()
		return io.EOF
	}
	// Publish the socket before route I/O: CANCEL can close it immediately, and
	// even a failed route cleanup remains reachable through this retained call.
	if len(call.invite.Body) > 0 {
		if err := media.configureRemoteContext(ctx, call.invite.Body); err != nil {
			_ = media.Close()
			return err
		}
	}
	return nil
}

func (session *Session) rejectCellularAnswerSetup(call *imsCall) {
	call.replyMu.Lock()
	defer call.replyMu.Unlock()
	session.callMu.Lock()
	canReject := !call.terminated && call.inviteStatus < 200
	session.callMu.Unlock()
	if !canReject {
		return
	} // CANCEL/hangup already committed the final response.
	response, err := buildSIPResponseWithBody(call.invite, 488, session.fromTag, nil)
	if err != nil {
		session.finishCall(call.callID, "failed", 0, "IMS media setup failed")
		return
	}
	session.callMu.Lock()
	call.inviteStatus, call.inviteResponse = 488, append([]byte(nil), response...)
	session.callMu.Unlock()
	session.finishCall(call.callID, "failed", 488, "IMS media setup failed")
	_ = call.respond(response)
}

// Only one worker and four queued SDP changes per call. The SIP receive loop
// never waits for WDS/route I/O; termination cancels the worker's current job.
func (session *Session) queueCellularMediaJob(call *imsCall, job func()) bool {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	return session.queueCellularMediaJobLocked(call, job)
}

// callMu also orders CSeq reservation and queue insertion across both receive
// sockets. A later CSeq cannot enqueue ahead of an earlier accepted request.
func (session *Session) queueCellularMediaJobLocked(call *imsCall, job func()) bool {
	if call.terminated || call.public.EndedAt != nil {
		return false
	}
	ctx := session.callMediaContextLocked(call)
	start := call.mediaJobs == nil
	if start {
		call.mediaJobs = make(chan func(), 4)
	}
	jobs := call.mediaJobs
	select {
	case jobs <- job:
	default:
		return false
	}
	if start {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case next := <-jobs:
					if ctx.Err() != nil {
						return
					}
					next()
				}
			}
		}()
	}
	return true
}

type dialogOfferKey struct {
	sequence                 uint32
	method, branch, via, uri string
}

type dialogOfferTransaction struct {
	key                  dialogOfferKey
	pending              bool
	response             []byte
	responders           []func([]byte) error
	request              *sipRequest
	ctx                  context.Context
	cancel               context.CancelFunc
	commitMu             sync.Mutex // never held across AcquireMediaRoute or response writes
	committed, cancelled bool
}

const maxDialogOfferTransactions = 8
const maxDialogOfferResponders = 4

// callMu must be held. Identity checks precede even cached replay: a forged
// Call-ID alone must never authorize media changes or disclose dialog SDP.
func (session *Session) admitDialogOfferLocked(call *imsCall, request *sipRequest, respond func([]byte) error) (*dialogOfferTransaction, []byte, int) {
	sequence, method, err := cseqNumber(request.value("CSeq"))
	if call == nil || call.terminated || call.public.EndedAt != nil || call.public.State != "active" || call.media == nil ||
		strings.TrimSpace(request.value("Call-ID")) != call.callID ||
		headerURI(request.value("From")) != headerURI(call.to) || headerURI(request.value("To")) != headerURI(call.from) ||
		headerParameter(request.value("From"), "tag") == "" || headerParameter(request.value("To"), "tag") == "" ||
		headerParameter(request.value("From"), "tag") != headerParameter(call.to, "tag") ||
		headerParameter(request.value("To"), "tag") != headerParameter(call.from, "tag") {
		return nil, nil, 481
	}
	branch := headerParameter(request.value("Via"), "branch")
	if err != nil || sequence == 0 || sequence >= 1<<31 || method != request.Method || (method != "UPDATE" && method != "INVITE") || branch == "" {
		return nil, nil, 500
	}
	key := dialogOfferKey{sequence: sequence, method: method, branch: branch, via: request.value("Via"), uri: request.URI}
	if existing := call.dialogOffers[key]; existing != nil {
		if !existing.pending {
			return nil, append([]byte(nil), existing.response...), 0
		}
		if len(existing.responders) < maxDialogOfferResponders {
			existing.responders = append(existing.responders, respond)
		}
		return nil, nil, 0
	}
	if sequence <= call.remoteCSeq {
		return nil, nil, 500
	}
	if call.dialogOffers == nil {
		call.dialogOffers = make(map[dialogOfferKey]*dialogOfferTransaction)
	}
	if len(call.dialogOffers) >= maxDialogOfferTransactions {
		var oldest *dialogOfferTransaction
		for _, entry := range call.dialogOffers {
			if !entry.pending && (oldest == nil || entry.key.sequence < oldest.key.sequence) {
				oldest = entry
			}
		}
		if oldest == nil {
			return nil, nil, 488
		}
		delete(call.dialogOffers, oldest.key)
	}
	ctx, cancel := context.WithCancel(session.callMediaContextLocked(call))
	transaction := &dialogOfferTransaction{key: key, pending: true, responders: []func([]byte) error{respond}, request: request, ctx: ctx, cancel: cancel}
	call.dialogOffers[key] = transaction
	call.remoteCSeq = sequence
	if call.localOfferPending {
		response, err := buildSIPResponseWithBody(request, 491, session.fromTag, nil)
		transaction.pending = false
		transaction.response = response
		transaction.responders = nil
		cancel()
		if err != nil {
			return nil, nil, 500
		}
		return nil, append([]byte(nil), response...), 0
	}
	return transaction, nil, 0
}

func (session *Session) completeDialogOffer(call *imsCall, transaction *dialogOfferTransaction, request *sipRequest, status int, body []byte) {
	var extra []string
	if status == 200 && request.Method == "INVITE" {
		extra = append(extra, session.dialogContactHeader())
	}
	response, err := buildSIPResponseWithBody(request, status, session.fromTag, body, extra...)
	if err != nil {
		response = nil
	}
	transaction.commitMu.Lock()
	if transaction.cancelled {
		transaction.commitMu.Unlock()
		return
	}
	responders := session.cacheDialogOfferResponse(call, transaction, response)
	transaction.cancel()
	transaction.commitMu.Unlock()
	if len(response) != 0 {
		for _, respond := range responders {
			_ = respond(response)
		}
	}
}

// commitMu must be held; callMu is acquired only for the bounded cache update.
func (session *Session) cacheDialogOfferResponse(call *imsCall, transaction *dialogOfferTransaction, response []byte) []func([]byte) error {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if !transaction.pending {
		return nil
	}
	transaction.pending = false
	transaction.response = append([]byte(nil), response...)
	responders := transaction.responders
	transaction.responders = nil
	return responders
}

func (transaction *dialogOfferTransaction) commitMedia(apply func() error) error {
	transaction.commitMu.Lock()
	defer transaction.commitMu.Unlock()
	if transaction.cancelled {
		return context.Canceled
	}
	if err := apply(); err != nil {
		return err
	}
	// This is the final-200 commitment. A later CANCEL may acknowledge its
	// own request, but cannot undo the installed target or cause a 487.
	transaction.committed = true
	return nil
}

func (transaction *dialogOfferTransaction) mediaCommitted() bool {
	transaction.commitMu.Lock()
	defer transaction.commitMu.Unlock()
	return transaction.committed
}

func (session *Session) handleDialogOfferCancel(call *imsCall, request *sipRequest, respond func([]byte) error) bool {
	session.callMu.Lock()
	var transaction *dialogOfferTransaction
	for _, entry := range call.dialogOffers {
		if entry.key.method == "INVITE" && sameInviteTransaction(entry.request, request) && entry.request.value("Via") == request.value("Via") {
			transaction = entry
			break
		}
	}
	session.callMu.Unlock()
	if transaction == nil {
		return false
	}
	response, err := buildSIPResponseWithBody(transaction.request, 487, session.fromTag, nil)
	if err != nil {
		return false
	}
	transaction.commitMu.Lock()
	session.callMu.Lock()
	final := !transaction.pending
	session.callMu.Unlock()
	var responders []func([]byte) error
	if !transaction.committed && !final {
		transaction.cancelled = true
		transaction.cancel()
		responders = session.cacheDialogOfferResponse(call, transaction, response)
	}
	transaction.commitMu.Unlock()
	session.respondCall(request, respond, 200)
	for _, send := range responders {
		_ = send(response)
	}
	return true
}
