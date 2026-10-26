package sipgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/store"
)

// The pause is before one REAL coherent SQLite read. It never fabricates a
// settings value, ledger, source guard, claim, CAS result or wire permission.
type smsCanonicalWirePause struct {
	*store.Store
	mu      sync.Mutex
	armed   bool
	reached chan struct{}
	release <-chan struct{}
}

func (database *smsCanonicalWirePause) ReadSMSForwardWireSnapshot(ctx context.Context, guard *store.SMSForwardInboxGuard) (store.SMSForwardWireSnapshot, error) {
	database.mu.Lock()
	armed, reached, release := database.armed, database.reached, database.release
	database.armed = false
	database.mu.Unlock()
	if armed {
		close(reached)
		select {
		case <-release:
		case <-ctx.Done():
			return store.SMSForwardWireSnapshot{}, ctx.Err()
		}
	}
	return database.Store.ReadSMSForwardWireSnapshot(ctx, guard)
}

func TestSMSCanonicalSnapshotEachWireRequiresTheSameOwnedUnknownClaimForBoundAndIndependentSources(t *testing.T) {
	for _, independent := range []bool{false, true} {
		for _, mutation := range []string{"removed", "terminal", "token", "identity", "canonical", "body", "expired", "extended", "source"} {
			t.Run(fmt.Sprintf("independent=%t/%s", independent, mutation), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				stores := smsCASStores(t)
				wrapped := &smsCanonicalWirePause{Store: stores[0]}
				backend := newSyntheticSMS()
				backend.subscription = hashCallOperation(smsCanonicalICCID)
				manager := New(ctx, wrapped, WithCarrierSMSBackend(backend))
				t.Cleanup(manager.Close)
				if view, err := manager.Apply(ctx, smsAuthorityConfig(t)); err != nil || !view.Status.Running {
					t.Fatal("could not start the actual wire-snapshot Manager:", err)
				}
				phone := registerMessagePeer(t, manager.runtime, "1001")
				message := IncomingSMS{ID: "own-claim-original", DeviceID: "module-a", Subscription: smsCanonicalICCID,
					Sender: "+15550000001", Text: "synthetic body whose claim must still be owned"}
				key := hashCallOperation(message.DeviceID + "\x00" + message.Subscription + "\x00" + message.ID)
				if independent {
					if err := manager.QueueSMS(ctx, 1, &message); err != nil {
						t.Fatal(err)
					}
				} else {
					saved, err := stores[0].SaveSMSMessage(ctx, store.SMSMessage{MessageID: message.ID, DeviceID: message.DeviceID,
						ICCID: smsCanonicalICCID, Peer: message.Sender, Direction: "inbound", Body: message.Text, Source: "ims", PartsTotal: 1})
					if err != nil {
						t.Fatal(err)
					}
					key = hashCallOperation("sms-source-v1\x00" + store.SMSForwardSourceKey(saved))
				}
				gate := newSMSCASRelease()
				t.Cleanup(gate.close)
				wrapped.mu.Lock()
				wrapped.armed, wrapped.reached, wrapped.release = true, make(chan struct{}), gate.ch
				reached := wrapped.reached
				wrapped.mu.Unlock()
				done := make(chan error, 1)
				go func() { done <- manager.DeliverSMSOnce(ctx) }()
				smsCASWait(t, ctx, reached, 1, "post-claim coherent snapshot admission")
				before, err := stores[1].AppSetting(ctx, SMSLedgerSettingKey)
				if err != nil {
					t.Fatal(err)
				}
				var state smsLedger
				if json.Unmarshal(before.Value, &state) != nil || state.Version != 3 {
					t.Fatal("the actual claim did not commit a private v3 ledger")
				}
				in := state.Inbox[key]
				target := in.Targets["1001"]
				if target.Code != 504 || !validSMSAttemptToken(target.AttemptToken) || in.CanonicalReply == nil {
					t.Fatal("test pause was not after the owned durable Unknown claim")
				}
				switch mutation {
				case "removed":
					delete(state.Inbox, key)
				case "terminal":
					target.Code = 200
				case "token":
					target.AttemptToken = "dddddddddddddddddddddddddddddddd"
				case "identity":
					target.Identity = hashCallOperation("a different original recipient identity")
				case "canonical":
					reply := "+15550000002"
					in.CanonicalReply = &reply
				case "body":
					in.Message.Text = "synthetic replacement payload with the same source reference"
				case "expired":
					in.Until = time.Now().Add(-time.Minute)
				case "extended":
					in.Until = in.Until.Add(time.Hour)
				case "source":
					if independent {
						in.SourceIndependent = false // An unbound legacy record grants no replacement authority.
					} else {
						in.SourceKey = hashCallOperation("another original receive source")
					}
				}
				if mutation != "removed" {
					in.Targets["1001"] = target
					state.Inbox[key] = in
				}
				replacement, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if changed, err := stores[1].CompareSMSLedger(ctx, before.Value, replacement); err != nil || !changed {
					t.Fatal("real second Store could not replace only the exact observed claim:", err)
				}
				gate.close()
				smsCASDeliveryResult(t, ctx, done)
				smsCanonicalNoUDP(t, phone)
				after, err := stores[1].AppSetting(ctx, SMSLedgerSettingKey)
				if err != nil || !bytes.Equal(after.Value, replacement) {
					t.Fatal("stale wire worker completed, revived or overwrote the replacement claim:", err)
				}
				// The same live registration must still accept an unrelated new
				// source, proving the negative result is not a disabled transport.
				fresh := message
				fresh.ID, fresh.Text = "own-claim-fresh", "synthetic fresh source after the stale claim veto"
				if independent {
					if err := manager.QueueSMS(ctx, 2, &fresh); err != nil {
						t.Fatal(err)
					}
				} else if _, err := stores[0].SaveSMSMessage(ctx, store.SMSMessage{MessageID: fresh.ID, DeviceID: fresh.DeviceID,
					ICCID: smsCanonicalICCID, Peer: fresh.Sender, Direction: "inbound", Body: fresh.Text, Source: "ims", PartsTotal: 1}); err != nil {
					t.Fatal(err)
				}
				smsCanonicalDeliver(t, ctx, manager, phone, fresh.Text)
				if backend.count() != 0 {
					t.Fatal("an MT claim/source check submitted a carrier SMS")
				}
			})
		}
	}
}

func TestSMSCanonicalSnapshotOldWireSuccessCannotCompleteAReplacementCanonicalPayload(t *testing.T) {
	for _, independent := range []bool{false, true} {
		t.Run(fmt.Sprintf("independent=%t", independent), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			stores := smsCASStores(t)
			backend := newSyntheticSMS()
			backend.subscription = hashCallOperation(smsCanonicalICCID)
			manager := smsCanonicalStart(t, ctx, stores[0], backend)
			phone := registerMessagePeer(t, manager.runtime, "1001")
			message := IncomingSMS{ID: "canonical-completion-original", DeviceID: "module-a", Subscription: smsCanonicalICCID,
				Sender: "+15550000001", Text: "synthetic original payload awaiting its actual final response"}
			key := hashCallOperation(message.DeviceID + "\x00" + message.Subscription + "\x00" + message.ID)
			if independent {
				if err := manager.QueueSMS(ctx, 1, &message); err != nil {
					t.Fatal(err)
				}
			} else {
				saved, err := stores[0].SaveSMSMessage(ctx, store.SMSMessage{MessageID: message.ID, DeviceID: message.DeviceID,
					ICCID: smsCanonicalICCID, Peer: message.Sender, Direction: "inbound", Body: message.Text, Source: "ims", PartsTotal: 1})
				if err != nil {
					t.Fatal(err)
				}
				key = hashCallOperation("sms-source-v1\x00" + store.SMSForwardSourceKey(saved))
			}
			done := make(chan error, 1)
			go func() { done <- manager.DeliverSMSOnce(ctx) }()
			request, err := readPeerMessage(phone.conn)
			if err != nil || request == nil || string(request.Body()) != message.Text {
				t.Fatal("actual original payload did not reach the registered receiver:", err)
			}
			before, err := stores[1].AppSetting(ctx, SMSLedgerSettingKey)
			if err != nil {
				t.Fatal(err)
			}
			state := smsCASReadLedger(t, stores[1])
			in := state.Inbox[key]
			if in.Targets["1001"].Code != 504 || !validSMSAttemptToken(in.Targets["1001"].AttemptToken) {
				t.Fatal("original wire payload had no durable owned Unknown reservation")
			}
			reply := "+15550000002"
			in.CanonicalReply = &reply
			state.Inbox[key] = in
			replacement, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := stores[1].CompareSMSLedger(ctx, before.Value, replacement); err != nil || !changed {
				t.Fatal("could not replace only the observed original canonical payload:", err)
			}
			response := sip.NewResponseFromRequest(request, 200, "Test final for the old payload", nil)
			if _, err := phone.conn.Write([]byte(response.String())); err != nil {
				t.Fatal(err)
			}
			smsCASDeliveryResult(t, ctx, done)
			after, err := stores[1].AppSetting(ctx, SMSLedgerSettingKey)
			if err != nil || !bytes.Equal(after.Value, replacement) {
				t.Fatal("the old UDP final falsely accepted or overwrote the changed canonical payload:", err)
			}
			if err := manager.DeliverSMSOnce(ctx); err != nil {
				t.Fatal(err)
			}
			smsCanonicalNoUDP(t, phone)
			if backend.count() != 0 {
				t.Fatal("MT completion/canonical replacement submitted a carrier SMS")
			}
		})
	}
}

func TestSMSCanonicalSnapshotVersionThreeMissingNullOrMalformedReplyFailsClosedWithoutRewriting(t *testing.T) {
	for _, invalid := range []string{"missing", "null", "number", "national", "whitespace", "mixed-v2"} {
		t.Run(invalid, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			database := carrierTestStore(t)
			backend := newSyntheticSMS()
			backend.subscription = hashCallOperation(smsCanonicalICCID)
			manager := smsCanonicalStart(t, ctx, database, backend)
			phone := registerMessagePeer(t, manager.runtime, "1001")
			message := IncomingSMS{ID: "malformed-v3-source", DeviceID: "module-a", Subscription: smsCanonicalICCID,
				Sender: smsCanonicalNational, Text: "synthetic private malformed journal control"}
			if err := manager.QueueSMS(ctx, 1, &message); err != nil {
				t.Fatal(err)
			}
			value, err := database.AppSetting(ctx, SMSLedgerSettingKey)
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if json.Unmarshal(value.Value, &raw) != nil {
				t.Fatal("invalid real private ledger fixture")
			}
			key := hashCallOperation(message.DeviceID + "\x00" + message.Subscription + "\x00" + message.ID)
			in := raw["inbox"].(map[string]any)[key].(map[string]any)
			switch invalid {
			case "missing":
				delete(in, "canonical_reply")
			case "null":
				in["canonical_reply"] = nil
			case "number":
				in["canonical_reply"] = 8615550000001
			case "national":
				in["canonical_reply"] = smsCanonicalNational
			case "whitespace":
				in["canonical_reply"] = "+86" + smsCanonicalNational + " "
			case "mixed-v2":
				raw["version"] = 2
			}
			corrupt, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := database.CompareSMSLedger(ctx, value.Value, corrupt); err != nil || !changed {
				t.Fatal(err)
			}
			if _, err := manager.SMSCursor(ctx, 999); err == nil {
				t.Fatal("malformed/mixed canonical history was silently repaired or ignored")
			}
			if err := manager.DeliverSMSOnce(ctx); err == nil {
				t.Fatal("malformed/mixed canonical history did not reject private delivery")
			}
			smsCanonicalNoUDP(t, phone)
			after, err := database.AppSetting(ctx, SMSLedgerSettingKey)
			if err != nil || !bytes.Equal(after.Value, corrupt) || backend.count() != 0 {
				t.Fatal("failed canonical parsing changed private history or exposed a paid SMS:", err)
			}
		})
	}
}

func TestSMSCanonicalSnapshotExplicitEmptyReplyRemainsReadOnlyDespiteCurrentNationalPolicy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database := carrierTestStore(t)
	backend := newSyntheticSMS()
	backend.subscription = hashCallOperation(smsCanonicalICCID)
	manager := smsCanonicalStart(t, ctx, database, backend)
	phone := registerMessagePeer(t, manager.runtime, "1001")
	message := IncomingSMS{ID: "explicit-read-only-v3-source", DeviceID: "module-a", Subscription: smsCanonicalICCID,
		Sender: smsCanonicalNational, Text: "synthetic intentionally read-only v3 SMS"}
	if err := manager.QueueSMS(ctx, 1, &message); err != nil {
		t.Fatal(err)
	}
	value, err := database.AppSetting(ctx, SMSLedgerSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	state := smsCASReadLedger(t, database)
	key := hashCallOperation(message.DeviceID + "\x00" + message.Subscription + "\x00" + message.ID)
	in := state.Inbox[key]
	empty := ""
	in.CanonicalReply = &empty
	state.Inbox[key] = in
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := database.CompareSMSLedger(ctx, value.Value, data); err != nil || !changed {
		t.Fatal(err)
	}
	request := smsCanonicalDeliver(t, ctx, manager, phone, message.Text)
	alias := request.From().Address.User
	phone.messageChallenge(alias, "synthetic read-only reply attempt", "text/plain")
	requireSMSReplyWireRejected(t, phone.exchange(phone.messagePacket(alias, "synthetic read-only reply attempt", "text/plain", phone.messageAuthorization(alias), 2, "")))
	if backend.count() != 0 {
		t.Fatal("explicit empty v3 canonical reply was reinterpreted using the active country policy")
	}
}
