package sipgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/store"
)

func smsPeerLifecycleResult(t *testing.T, ctx context.Context, result <-chan bool, expected bool) {
	t.Helper()
	select {
	case ready := <-result:
		if ready != expected {
			t.Fatalf("actual runtime-owned readiness result=%t, want=%t", ready, expected)
		}
	case <-ctx.Done():
		t.Fatal("real readiness owner/waiter did not complete:", ctx.Err())
	}
}

func smsPeerLifecycleStart(ctx context.Context, runtime *runtime) <-chan bool {
	result := make(chan bool, 1)
	go func() { result <- runtime.smsPeerReady(ctx) }()
	return result
}

func smsPeerLifecycleReadProbe(t *testing.T, peer *net.UDPConn) (*sip.Request, *net.UDPAddr) {
	t.Helper()
	request, source, raw, err := smsPeerReadinessRead(peer, 2*time.Second)
	if err != nil {
		t.Fatal("strict fixed peer did not receive its actual OPTIONS:", err)
	}
	smsPeerReadinessRequireProbe(t, request, raw, smsCaptureMessage("probe-privacy-control", "synthetic private body not admitted to a readiness probe"))
	if request.Method != sip.OPTIONS || request.CallID() == nil || request.Via() == nil {
		t.Fatal("readiness did not use a distinct bodyless OPTIONS transaction")
	}
	return request, source
}

func smsPeerLifecycleWaitCacheExpiry(t *testing.T, ctx context.Context, runtime *runtime) {
	t.Helper()
	runtime.mu.Lock()
	until := runtime.smsPeerProbeUntil
	runtime.mu.Unlock()
	if until.IsZero() || time.Until(until) > 2*time.Second {
		t.Fatal("readiness did not retain its bounded one-second positive/negative cache")
	}
	timer := time.NewTimer(time.Until(until) + 25*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal("real readiness cache did not naturally expire:", ctx.Err())
	}
}

func smsPeerLifecycleDrainProbe(t *testing.T, peer *net.UDPConn, original *sip.Request) {
	t.Helper()
	for {
		request, _, raw, err := smsPeerReadinessRead(peer, 75*time.Millisecond)
		if err != nil {
			if timeout, valid := err.(net.Error); valid && timeout.Timeout() {
				return
			}
			t.Fatal(err)
		}
		smsPeerReadinessRequireProbe(t, request, raw, smsCaptureMessage("drain-control", "synthetic private body never sent in readiness"))
		if request.Method != sip.OPTIONS || request.CallID() == nil || request.CallID().Value() != original.CallID().Value() {
			t.Fatal("one active owner spawned another alias-specific probe or exposed a MESSAGE")
		}
	}
}

func TestSMSPeerReadinessLifecyclePositiveAndNegativeCacheExpireWithoutRenewal(t *testing.T) {
	for _, initiallyReady := range []bool{false, true} {
		t.Run(fmt.Sprintf("ready=%t", initiallyReady), func(t *testing.T) {
			ctx, _, manager, backend, peer := smsPeerReadinessFixture(t, false)
			runtime := manager.runtime
			firstResult := smsPeerLifecycleStart(ctx, runtime)
			first, source := smsPeerLifecycleReadProbe(t, peer)
			status := 503
			if initiallyReady {
				status = 200
			}
			if err := fixedPeerResponse(peer, first, source, status); err != nil {
				t.Fatal(err)
			}
			smsPeerLifecycleResult(t, ctx, firstResult, initiallyReady)
			runtime.mu.Lock()
			until, cached, owner := runtime.smsPeerProbeUntil, runtime.smsPeerProbeReady, runtime.smsPeerProbe
			runtime.mu.Unlock()
			if owner != nil || cached != initiallyReady || !until.After(time.Now()) {
				t.Fatal("completed owner did not publish and retire its bounded cache")
			}
			cachedResult := smsPeerLifecycleStart(ctx, runtime)
			select {
			case ready := <-cachedResult:
				if ready != initiallyReady {
					t.Fatal("short cache changed the actual readiness result")
				}
			case <-time.After(150 * time.Millisecond):
				t.Fatal("an unexpired cache unnecessarily waited for another network probe")
			}
			smsCaptureWireNoUDP(t, peer)
			runtime.mu.Lock()
			unchanged := runtime.smsPeerProbeUntil.Equal(until)
			runtime.mu.Unlock()
			if !unchanged {
				t.Fatal("reading the readiness cache renewed its expiration")
			}
			smsPeerLifecycleWaitCacheExpiry(t, ctx, runtime)
			nextResult := smsPeerLifecycleStart(ctx, runtime)
			next, destination := smsPeerLifecycleReadProbe(t, peer)
			if next.CallID().Value() == first.CallID().Value() || next.Via().Params.GetOr("branch", "") == first.Via().Params.GetOr("branch", "") {
				t.Fatal("naturally expired cache reused the retired response identity")
			}
			nextStatus := 200
			if initiallyReady {
				nextStatus = 503
			}
			if err := fixedPeerResponse(peer, next, destination, nextStatus); err != nil {
				t.Fatal(err)
			}
			smsPeerLifecycleResult(t, ctx, nextResult, !initiallyReady)
			if backend.count() != 0 {
				t.Fatal("cache/probe lifecycle invoked the paid SMS backend")
			}
		})
	}
	t.Run("runtime-restart-does-not-inherit-positive-cache", func(t *testing.T) {
		ctx, _, manager, _, peer := smsPeerReadinessFixture(t, false)
		previous := manager.runtime
		result := smsPeerLifecycleStart(ctx, previous)
		probe, source := smsPeerLifecycleReadProbe(t, peer)
		if err := fixedPeerResponse(peer, probe, source, 200); err != nil {
			t.Fatal(err)
		}
		smsPeerLifecycleResult(t, ctx, result, true)
		config := manager.Snapshot().Config
		config.Listen = unusedAddress(t)
		if _, err := manager.Apply(ctx, config); err != nil {
			t.Fatal(err)
		}
		current := manager.runtime
		if current == nil || current == previous || !previous.closed.Load() {
			t.Fatal("real Manager update did not replace its owned runtime")
		}
		current.mu.Lock()
		cold := current.smsPeerProbe == nil && current.smsPeerProbeUntil.IsZero()
		current.mu.Unlock()
		if !cold {
			t.Fatal("a replacement runtime inherited its predecessor's positive readiness cache")
		}
		nextResult := smsPeerLifecycleStart(ctx, current)
		next, destination := smsPeerLifecycleReadProbe(t, peer)
		if next.CallID().Value() == probe.CallID().Value() {
			t.Fatal("replacement runtime reused a retired readiness transaction")
		}
		if err := fixedPeerResponse(peer, next, destination, 503); err != nil {
			t.Fatal(err)
		}
		smsPeerLifecycleResult(t, ctx, nextResult, false)
	})
}

func TestSMSPeerReadinessLifecycleSingleflightWaiterCancellationDoesNotKillAnotherWaiter(t *testing.T) {
	ctx, _, manager, backend, peer := smsPeerReadinessFixture(t, false)
	runtime := manager.runtime
	firstContext, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	firstResult := smsPeerLifecycleStart(firstContext, runtime)
	probe, source := smsPeerLifecycleReadProbe(t, peer)
	runtime.mu.Lock()
	owner := runtime.smsPeerProbe
	runtime.mu.Unlock()
	if owner == nil {
		t.Fatal("actual OPTIONS has no runtime-owned completion signal")
	}
	secondResult := smsPeerLifecycleStart(ctx, runtime)
	cancelFirst()
	smsPeerLifecycleResult(t, ctx, firstResult, false)
	runtime.mu.Lock()
	sameOwner := runtime.smsPeerProbe == owner
	runtime.mu.Unlock()
	if !sameOwner || owner.ctx.Err() != nil {
		t.Fatal("one cancelled waiter killed or replaced the shared runtime owner")
	}
	if err := fixedPeerResponse(peer, probe, source, 200); err != nil {
		t.Fatal(err)
	}
	smsPeerLifecycleResult(t, ctx, secondResult, true)
	smsPeerLifecycleDrainProbe(t, peer, probe)
	if backend.count() != 0 {
		t.Fatal("independent readiness waiters reached a carrier SMS sender")
	}
}

func TestSMSPeerReadinessLifecycleCloseWaitsForOwnerAndRejectsLateOrSpoofedIdentity(t *testing.T) {
	t.Run("close", func(t *testing.T) {
		ctx, _, manager, _, peer := smsPeerReadinessFixture(t, false)
		runtime := manager.runtime
		result := smsPeerLifecycleStart(ctx, runtime)
		probe, source := smsPeerLifecycleReadProbe(t, peer)
		runtime.mu.Lock()
		owner := runtime.smsPeerProbe
		runtime.mu.Unlock()
		if owner == nil {
			t.Fatal("real pending probe has no owned lifecycle")
		}
		closed := make(chan struct{})
		go func() { manager.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(1500 * time.Millisecond):
			t.Fatal("Manager.Close did not cancel and await its bounded readiness owner")
		}
		select {
		case <-owner.done:
		default:
			t.Fatal("Close returned before the owner retired its response wire")
		}
		smsPeerLifecycleResult(t, ctx, result, false)
		smsPeerLifecycleDrainProbe(t, peer, probe)
		if err := fixedPeerResponse(peer, probe, source, 200); err != nil {
			t.Fatal(err)
		}
		runtime.mu.Lock()
		retired := runtime.smsPeerProbe == nil && !runtime.smsPeerProbeReady
		runtime.mu.Unlock()
		if !runtime.closed.Load() || !retired || runtime.smsPeerReady(ctx) {
			t.Fatal("late final after Close revived a positive readiness owner/cache")
		}
	})
	t.Run("spoof-and-retired-transaction", func(t *testing.T) {
		ctx, _, manager, _, peer := smsPeerReadinessFixture(t, false)
		runtime := manager.runtime
		result := smsPeerLifecycleStart(ctx, runtime)
		probe, source := smsPeerLifecycleReadProbe(t, peer)
		foreign := newMessagePeerSocket(t)
		if err := fixedPeerResponse(foreign, probe, source, 200); err != nil {
			t.Fatal(err)
		}
		wrong := sip.NewResponseFromRequest(probe, 200, "Synthetic wrong readiness transaction", nil)
		callID := sip.CallIDHeader("unrelated-readiness-transaction")
		wrong.ReplaceHeader(&callID)
		if _, err := peer.WriteToUDP([]byte(wrong.String()), source); err != nil {
			t.Fatal(err)
		}
		smsPeerLifecycleResult(t, ctx, result, false)
		smsPeerLifecycleDrainProbe(t, peer, probe)
		runtime.mu.Lock()
		cached := runtime.smsPeerProbeReady
		runtime.mu.Unlock()
		if cached {
			t.Fatal("foreign source or mismatched Call-ID created a positive readiness cache")
		}
		smsPeerLifecycleWaitCacheExpiry(t, ctx, runtime)
		nextResult := smsPeerLifecycleStart(ctx, runtime)
		next, destination := smsPeerLifecycleReadProbe(t, peer)
		if err := fixedPeerResponse(peer, probe, source, 200); err != nil {
			t.Fatal(err)
		}
		select {
		case <-nextResult:
			t.Fatal("a final on the retired probe identity completed its replacement owner")
		case <-time.After(75 * time.Millisecond):
		}
		if err := fixedPeerResponse(peer, next, destination, 200); err != nil {
			t.Fatal(err)
		}
		smsPeerLifecycleResult(t, ctx, nextResult, true)
	})
}

func TestSMSPeerReadinessLifecycleAwaitedProbeCannotInheritRevokedGrantOrChangedInbox(t *testing.T) {
	for _, mutation := range []string{"receive-grant", "peer-grant", "delete", "body"} {
		t.Run(mutation, func(t *testing.T) {
			ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, false)
			saved, original := smsPeerReadinessSaved(t, ctx, stores, "lifecycle-source-change-during-probe")
			done := make(chan error, 1)
			go func() { done <- manager.DeliverSMSOnce(ctx) }()
			probe, source := smsPeerLifecycleReadProbe(t, peer)
			smsPeerReadinessQueued(t, stores[1], saved, original)
			if mutation == "receive-grant" || mutation == "peer-grant" {
				second := smsAuthoritySecondManager(t, ctx, stores[1], backend)
				changed := second.Snapshot().Config
				changed.Listen = unusedAddress(t)
				if mutation == "receive-grant" {
					changed.CarrierLines[0].AllowSMSReceive = false
				} else {
					changed.Peer.AllowSMSToPeer = false
				}
				if _, err := second.Apply(ctx, changed); err != nil {
					t.Fatal("second real Manager could not persist the revocation:", err)
				}
			} else if mutation == "delete" {
				if err := stores[1].DeleteSMSMessage(ctx, saved.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				saved.Body = "synthetic edited actual Inbox while readiness waits"
				if _, err := stores[1].SaveSMSMessage(ctx, saved); err != nil {
					t.Fatal(err)
				}
			}
			if err := fixedPeerResponse(peer, probe, source, 200); err != nil {
				t.Fatal(err)
			}
			smsCASDeliveryResult(t, ctx, done)
			smsPeerLifecycleDrainProbe(t, peer, probe)
			_, cancelled := smsCaptureSavedInbox(t, stores[1], saved)
			target := cancelled.Targets[peerSMSRecipientKey("sms-in")]
			if target.Code != 403 || target.AttemptToken != "" || !cancelled.Until.Equal(original.Until) ||
				!smsInboxSameSource(cancelled, original) || backend.count() != 0 {
				t.Fatal("probe success inherited changed authority/source, claimed payload or renewed the original TTL")
			}
		})
	}
}

func TestSMSPeerReadinessLifecycleProbeAndAuthenticationShareTheOriginalMessageDeadline(t *testing.T) {
	for _, cutoff := range []string{"parent-expired", "parent-cancelled", "negative-message-budget"} {
		t.Run(cutoff, func(t *testing.T) {
			ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, false)
			saved, original := smsPeerReadinessSaved(t, ctx, stores, "cutoff-before-any-readiness-or-claim")
			callContext := ctx
			var cancel context.CancelFunc
			switch cutoff {
			case "parent-expired":
				callContext, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			case "parent-cancelled":
				callContext, cancel = context.WithCancel(ctx)
				cancel()
			case "negative-message-budget":
				manager.runtime.mu.Lock()
				manager.runtime.messageTimeout = -time.Millisecond
				manager.runtime.mu.Unlock()
			}
			if cancel != nil {
				defer cancel()
			}
			if err := manager.DeliverSMSOnce(callContext); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("expired delivery failed with an unrelated state error:", err)
			}
			smsCaptureWireNoUDP(t, peer)
			smsPeerReadinessQueued(t, stores[1], saved, original)
			manager.runtime.mu.Lock()
			untouched := manager.runtime.smsPeerProbe == nil && manager.runtime.smsPeerProbeUntil.IsZero()
			manager.runtime.mu.Unlock()
			if !untouched || backend.count() != 0 {
				t.Fatal("an already expired/cancelled budget started readiness or reached the paid transport")
			}
		})
	}
	ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, false)
	budget := 2500 * time.Millisecond
	manager.runtime.mu.Lock()
	manager.runtime.messageTimeout = budget
	manager.runtime.mu.Unlock()
	saved, original := smsPeerReadinessSaved(t, ctx, stores, "lifecycle-probe-auth-one-original-budget")
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- manager.DeliverSMSOnce(ctx) }()
	probe, source := smsPeerLifecycleReadProbe(t, peer)
	// Withhold a REAL readiness response for part of the caller's budget.
	// The owner remains valid and the persisted original plan remains Queued.
	delay := time.NewTimer(time.Until(started.Add(400 * time.Millisecond)))
	defer delay.Stop()
	select {
	case <-delay.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	smsPeerReadinessQueued(t, stores[1], saved, original)
	if err := fixedPeerResponse(peer, probe, source, 200); err != nil {
		t.Fatal(err)
	}
	first, _, _, err := smsPeerReadinessRead(peer, 2*time.Second)
	if err != nil || first == nil || first.Method != sip.MESSAGE {
		t.Fatal("valid delayed readiness did not release its first actual MESSAGE:", err)
	}
	repeated, destination, _, err := smsPeerReadinessRead(peer, 2*time.Second)
	if err != nil || repeated == nil || repeated.Method != sip.MESSAGE || repeated.String() != first.String() {
		t.Fatal("actual MESSAGE did not retain its permitted T1 retransmission identity:", err)
	}
	if err := fixedPeerResponse(peer, repeated, destination, 407, sip.NewHeader("Proxy-Authenticate", `Digest realm="`+messagePeerRealm+`",nonce="synthetic-message-nonce",algorithm=SHA-256,qop="auth"`)); err != nil {
		t.Fatal(err)
	}
	authenticated, _, _, err := smsPeerReadinessRead(peer, 2*time.Second)
	if err != nil || authenticated == nil || authenticated.Method != sip.MESSAGE {
		t.Fatal("actual challenged MESSAGE did not authenticate:", err)
	}
	if err := verifyPeerMessageDigest(authenticated, "Proxy-Authorization"); err != nil {
		t.Fatal(err)
	}
	limit := time.NewTimer(time.Until(started.Add(budget + 300*time.Millisecond)))
	defer limit.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-limit.C:
		t.Fatal("readiness or authentication restarted the original total MESSAGE budget")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, unknown := smsCaptureSavedInbox(t, stores[1], saved)
	target := unknown.Targets[peerSMSRecipientKey("sms-in")]
	if target.Code != 504 || !validSMSAttemptToken(target.AttemptToken) || !unknown.Until.Equal(original.Until) || backend.count() != 0 {
		t.Fatal("overall deadline changed the owned Unknown, original TTL or paid carrier state")
	}
}

func TestSMSPeerReadinessLifecycleManyPeerAliasesYieldToRegisteredLocalRecipient(t *testing.T) {
	ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, true)
	config := manager.Snapshot().Config
	for index := 0; index < 18; index++ {
		line := config.CarrierLines[0]
		line.DeviceID, line.Label, line.Default, line.Prefix = fmt.Sprintf("module-extra-%d", index), "Synthetic extra module", false, ""
		line.SMSRecipients = []string{"1001"}
		config.CarrierLines = append(config.CarrierLines, line)
		config.Peer.IncomingRoutes = append(config.Peer.IncomingRoutes, PeerIncomingRoute{DeviceID: line.DeviceID, SMSTarget: fmt.Sprintf("sms-alias-%d", index)})
	}
	if _, err := manager.Apply(ctx, config); err != nil {
		t.Fatal(err)
	}
	phone := registerMessagePeer(t, manager.runtime, "1001")
	var saved []store.SMSMessage
	var original []smsInbox
	for index := 0; index < 18; index++ {
		message := smsCaptureMessage(fmt.Sprintf("many-alias-source-%d", index), fmt.Sprintf("synthetic independent alias body %d", index))
		message.DeviceID = fmt.Sprintf("module-extra-%d", index)
		value, err := stores[0].SaveSMSMessage(ctx, message)
		if err != nil {
			t.Fatal(err)
		}
		_, in := smsCaptureSavedInbox(t, stores[1], value)
		saved, original = append(saved, value), append(original, in)
	}
	local := replyOneMessage(phone.conn, 200)
	done := make(chan error, 1)
	go func() { done <- manager.DeliverSMSOnce(ctx) }()
	probe, _ := smsPeerLifecycleReadProbe(t, peer)
	select {
	case actual := <-local:
		if actual.err != nil || actual.request == nil || actual.request.Method != sip.MESSAGE {
			t.Fatal("multiple peer aliases prevented the independent real local delivery:", actual.err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("peer aliases occupied the worker slots needed by an online local extension")
	}
	smsCASDeliveryResult(t, ctx, done)
	smsPeerLifecycleDrainProbe(t, peer, probe)
	acceptedLocal := 0
	for index, value := range saved {
		_, current := smsCaptureSavedInbox(t, stores[1], value)
		target := current.Targets[peerSMSRecipientKey(fmt.Sprintf("sms-alias-%d", index))]
		if target.Code != 0 || target.AttemptToken != "" || !current.Until.Equal(original[index].Until) {
			t.Fatal("unanswered alias-specific readiness consumed a private claim or renewed its expiry")
		}
		if current.Targets["1001"].Code == 200 {
			acceptedLocal++
		}
	}
	if acceptedLocal != 1 || backend.count() != 0 {
		t.Fatal("alias isolation lost the single allowed local delivery or reached a paid carrier")
	}
}

func TestSMSPeerReadinessLifecycleDirectGuardedPeerWireStartsWithMESSAGEWithoutAdapter(t *testing.T) {
	ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, false)
	saved, in := smsPeerReadinessSaved(t, ctx, stores, "strict-direct-wire-original-source")
	key := hashCallOperation("sms-source-v1\x00" + store.SMSForwardSourceKey(saved))
	user := peerSMSRecipientKey("sms-in")
	before, err := stores[0].AppSetting(ctx, SMSLedgerSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	state := smsCASReadLedger(t, stores[0])
	token, err := newSMSAttemptToken()
	if err != nil {
		t.Fatal(err)
	}
	target := in.Targets[user]
	target.Code, target.AttemptToken = 504, token
	in.Targets[user] = target
	state.Inbox[key] = in
	after, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := stores[0].CompareSMSLedgerWithInboxGuard(ctx, before.Value, after,
		store.SMSForwardInboxGuard{ID: in.InboxID, Fingerprint: in.SourceFingerprint}); err != nil || !changed {
		t.Fatal("real Store could not FULL-commit the explicit direct-wire source claim:", err)
	}
	runtime := manager.runtime
	destination, valid := runtime.peerDeliveryTarget("sms-in")
	if !valid {
		t.Fatal("real admitted Manager has no exact peer destination")
	}
	incoming := sip.NewRequest(sip.MESSAGE, sip.Uri{Scheme: "sip", Host: runtime.identityDomain()})
	maximum := sip.MaxForwardsHeader(70)
	incoming.AppendHeader(&maximum)
	result := make(chan messageOutcome, 1)
	fingerprint := smsInboxPayloadFingerprint(in)
	go func() {
		result <- runtime.deliverMessage(ctx, incoming,
			Account{Username: smsReplyUser(saved.DeviceID, saved.ICCID, saved.Peer), DisplayName: saved.Peer}, Account{Username: "sms-in"}, destination,
			[]byte(saved.Body), func() bool {
				return manager.sms.smsWireClaimCurrent(ctx, runtime, key, user, token, target.Identity, fingerprint, in)
			})
	}()
	request, source, _, err := smsPeerReadinessRead(peer, 2*time.Second)
	if err != nil || request == nil || request.Method != sip.MESSAGE || string(request.Body()) != saved.Body {
		t.Fatal("strict direct guarded peer wire was incorrectly diverted through automatic OPTIONS:", err)
	}
	if err := fixedPeerResponse(peer, request, source, 200); err != nil {
		t.Fatal(err)
	}
	smsMessageWireResult(t, ctx, result, 200)
	runtime.mu.Lock()
	untouched := runtime.smsPeerProbe == nil && runtime.smsPeerProbeUntil.IsZero()
	runtime.mu.Unlock()
	actual, err := stores[1].AppSetting(ctx, SMSLedgerSettingKey)
	if err != nil || !bytes.Equal(actual.Value, after) || !untouched || backend.count() != 0 {
		t.Fatal("direct transport invented readiness/queue completion or changed the explicit owned claim:", err)
	}
}

type smsPeerLifecycleCallerMarker struct{}

// Observe the actual admission context, cancel the real standard parent, then
// delegate to SQLite with that same child. No Store result, authority or wire
// permission is fabricated. Caller values also distinguish its ancestry from
// a runtime-derived context even if an asynchronous cancellation callback races.
type smsPeerLifecycleAdmissionObserver struct {
	*store.Store
	mu              sync.Mutex
	cancel          context.CancelFunc
	marker          string
	called          bool
	callerValueSeen bool
	cancelSeen      bool
}

func (database *smsPeerLifecycleAdmissionObserver) AppSetting(ctx context.Context, key string) (store.AppSetting, error) {
	if key == SMSLedgerSettingKey {
		database.mu.Lock()
		if !database.called {
			database.called = true
			database.callerValueSeen = ctx.Value(smsPeerLifecycleCallerMarker{}) == database.marker
			database.cancel()
			database.cancelSeen = errors.Is(ctx.Err(), context.Canceled)
		}
		database.mu.Unlock()
	}
	return database.Store.AppSetting(ctx, key)
}

func TestSMSPeerReadinessLifecycleCallerCancellationSynchronouslyInvalidatesAdmissionContext(t *testing.T) {
	ctx, stores, manager, backend, peer := smsPeerReadinessFixture(t, false)
	saved, original := smsPeerReadinessSaved(t, ctx, stores, "synchronous-caller-cancellation-source")
	const marker = "synthetic original caller context ancestry"
	caller, cancel := context.WithCancel(context.WithValue(ctx, smsPeerLifecycleCallerMarker{}, marker))
	defer cancel()
	observer := &smsPeerLifecycleAdmissionObserver{Store: stores[0], cancel: cancel, marker: marker}
	manager.sms.db = observer
	key := hashCallOperation("sms-source-v1\x00" + store.SMSForwardSourceKey(saved))
	// Enter the real per-message admission directly: Manager.DeliverSMSOnce's
	// preliminary queue enumeration must not consume the cancellation observer.
	if err := manager.sms.deliverInbox(caller, manager.runtime, key, peerSMSRecipientKey("sms-in")); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal("actual cancelled admission returned an unrelated Store error:", err)
	}
	observer.mu.Lock()
	called, valueSeen, cancelled := observer.called, observer.callerValueSeen, observer.cancelSeen
	observer.mu.Unlock()
	if !called || !valueSeen || !cancelled || !errors.Is(caller.Err(), context.Canceled) {
		t.Fatalf("admission was not synchronously parented to the actual caller: read=%t caller-value=%t child-cancelled=%t", called, valueSeen, cancelled)
	}
	smsCaptureWireNoUDP(t, peer)
	smsPeerReadinessQueued(t, stores[1], saved, original)
	manager.runtime.mu.Lock()
	untouched := manager.runtime.smsPeerProbe == nil && manager.runtime.smsPeerProbeUntil.IsZero()
	manager.runtime.mu.Unlock()
	if !untouched || backend.count() != 0 {
		t.Fatal("synchronously cancelled admission started a probe, claim or paid carrier submission")
	}
}
