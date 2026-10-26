package sipgateway

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/emiago/sipgo/sip"
	"vocat/internal/store"
)

var smsMessagePartsPrefix = regexp.MustCompile(`^\[([1-9][0-9]*)/([1-9][0-9]*)\] `)

// All APIs/helpers below exist in the preceding accepted readiness archive.
// Only this file is added for the historical RED. No future segmentation API,
// automatic peer adapter, fabricated authorization or carrier is involved.
func smsMessagePartsFixture(t *testing.T) (context.Context, [2]*store.Store, *Manager, *syntheticSMS, *smsCaptureWireAuthorityPause) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	stores := smsCASStores(t)
	paused := &smsCaptureWireAuthorityPause{Store: stores[0]}
	manager, backend := smsCaptureWireManager(t, ctx, paused, nil)
	manager.runtime.mu.Lock()
	manager.runtime.messageTimeout = 2 * time.Second
	manager.runtime.mu.Unlock()
	return ctx, stores, manager, backend, paused
}

func smsMessagePartsExpected(text string) []string {
	var pieces []string
	var current strings.Builder
	for _, scalar := range text {
		if current.Len()+utf8.RuneLen(scalar) > 280 {
			pieces = append(pieces, current.String())
			current.Reset()
		}
		current.WriteRune(scalar)
	}
	if current.Len() != 0 {
		pieces = append(pieces, current.String())
	}
	return pieces
}

func smsMessagePartsReceive(connection *net.UDPConn) (*sip.Request, *net.UDPAddr, []byte, error) {
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return nil, nil, nil, err
	}
	buffer := make([]byte, 16*1024)
	count, source, err := connection.ReadFromUDP(buffer)
	if err != nil {
		return nil, nil, nil, err
	}
	raw := append([]byte(nil), buffer[:count]...)
	parsed, err := registrarParser().ParseSIP(raw)
	if err != nil {
		return nil, nil, raw, err
	}
	request, valid := parsed.(*sip.Request)
	if !valid {
		return nil, nil, raw, errors.New("actual receiver got a response instead of a MESSAGE request")
	}
	return request, source, raw, nil
}

func smsMessagePartsReply(t *testing.T, connection *net.UDPConn, request *sip.Request, source *net.UDPAddr, status int) bool {
	t.Helper()
	response := sip.NewResponseFromRequest(request, status, "Synthetic actual MT receiver", nil)
	raw := []byte(response.String())
	var count int
	var err error
	if remote := connection.RemoteAddr(); remote != nil {
		if source == nil || remote.String() != source.String() {
			t.Error("actual part source differs from the receiver's connected gateway")
			return false
		}
		count, err = connection.Write(raw)
	} else {
		count, err = connection.WriteToUDP(raw, source)
	}
	if err != nil {
		t.Error("could not return the actual part's SIP final:", err)
		return false
	}
	if count != len(raw) {
		t.Error("actual receiver did not write the entire SIP final")
		return false
	}
	return true
}

func smsMessagePartsStart(ctx context.Context, manager *Manager) (context.CancelFunc, <-chan error) {
	caller, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- manager.DeliverSMSOnce(caller) }()
	return cancel, done
}

func smsMessagePartsJoin(t *testing.T, ctx context.Context, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Error("actual multipart worker returned an unrelated storage error:", err)
		}
	case <-ctx.Done():
		t.Error("actual multipart worker did not converge:", ctx.Err())
	}
}

func smsMessagePartsCheck(t *testing.T, request *sip.Request, raw []byte, saved store.SMSMessage, expected []string, index int) bool {
	t.Helper()
	if request == nil || request.Method != sip.MESSAGE {
		t.Error("actual MT part was not a SIP MESSAGE")
		return false
	}
	body := string(request.Body())
	match := smsMessagePartsPrefix.FindStringSubmatchIndex(body)
	if match == nil {
		t.Errorf("actual MT was sent as one unlabelled body instead of independently identified [i/n] parts: bytes=%d", len(request.Body()))
		return false
	}
	number, _ := strconv.Atoi(body[match[2]:match[3]])
	total, _ := strconv.Atoi(body[match[4]:match[5]])
	content := body[match[1]:]
	if number != index+1 || total != len(expected) || content != expected[index] || len(content) > 280 || !utf8.ValidString(content) {
		t.Errorf("part label/content/UTF8 boundary differs from the raw-content-280 contract: index=%d total=%d content-bytes=%d", number, total, len(content))
	}
	// The human-readable [i/n] prefix is OUTSIDE the raw 280-byte content.
	if len(raw) > 1300 || request.From() == nil || request.To() == nil || request.CallID() == nil || request.Via() == nil || request.CSeq() == nil ||
		request.From().Address.User != smsReplyUser(saved.DeviceID, saved.ICCID, saved.Peer) || request.From().DisplayName != saved.Peer ||
		request.To().Address.User != "1001" || request.CSeq().SeqNo != 1 || request.CSeq().MethodName != sip.MESSAGE {
		t.Error("actual part lost its original reply source, recipient, independent transaction or safe UDP bound")
	}
	return true
}

func smsMessagePartsState(t *testing.T, database *store.Store, saved store.SMSMessage, original smsInbox, expectedCode int, token string) string {
	t.Helper()
	_, in := smsCaptureSavedInbox(t, database, saved)
	target := in.Targets["1001"]
	if target.Code != expectedCode || !validSMSAttemptToken(target.AttemptToken) || token != "" && target.AttemptToken != token ||
		!in.Until.Equal(original.Until) || !smsInboxSameSource(in, original) || smsInboxPayloadFingerprint(in) != smsInboxPayloadFingerprint(original) {
		t.Errorf("multipart durable outcome/owner/source/expiry changed: code=%d want=%d owned-token=%t original-expiry=%t", target.Code, expectedCode, validSMSAttemptToken(target.AttemptToken), in.Until.Equal(original.Until))
	}
	return target.AttemptToken
}

func smsMessagePartsNoReplay(t *testing.T, ctx context.Context, manager *Manager, phones ...*wireClient) {
	t.Helper()
	if err := manager.DeliverSMSOnce(ctx); err != nil {
		t.Error(err)
	}
	for _, phone := range phones {
		if err := phone.conn.SetReadDeadline(time.Now().Add(75 * time.Millisecond)); err != nil {
			t.Error(err)
			continue
		}
		if _, _, err := phone.conn.ReadFromUDP(make([]byte, 4096)); err == nil {
			t.Error("terminal/Unknown multipart SMS was automatically replayed or resumed")
		} else if timeout, valid := err.(net.Error); !valid || !timeout.Timeout() {
			t.Error("could not inspect the actual no-replay wire boundary:", err)
		}
	}
}

func TestSMSMessagePartsUnicodeLocalMTRequiresAllBoundedIndependentParts(t *testing.T) {
	ctx, stores, manager, backend, _ := smsMessagePartsFixture(t)
	phone := registerMessagePeer(t, manager.runtime, "1001")
	message := smsCaptureMessage("parts-unicode-1400-original", strings.Repeat("界", 466)+"ab") // Exactly 1400 UTF-8 bytes, below admission's 4096.
	saved, err := stores[0].SaveSMSMessage(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	_, original := smsCaptureSavedInbox(t, stores[1], saved)
	expected := smsMessagePartsExpected(saved.Body)
	cancel, done := smsMessagePartsStart(ctx, manager)
	defer cancel()
	ids, branches := map[string]bool{}, map[string]bool{}
	token := ""
	for index := range expected {
		request, source, raw, receiveErr := smsMessagePartsReceive(phone.conn)
		if receiveErr != nil {
			t.Error("admitted Unicode MT never reached the actual receiver as bounded parts:", receiveErr)
			cancel()
			smsMessagePartsJoin(t, ctx, done)
			smsMessagePartsState(t, stores[1], saved, original, 200, token)
			return
		}
		labelled := smsMessagePartsCheck(t, request, raw, saved, expected, index)
		token = smsMessagePartsState(t, stores[1], saved, original, 504, token)
		if request.CallID() != nil && request.Via() != nil {
			id, branch := request.CallID().Value(), request.Via().Params.GetOr("branch", "")
			if ids[id] || branches[branch] || branch == "" {
				t.Error("different application parts reused a SIP transaction identity")
			}
			ids[id], branches[branch] = true, true
		}
		if !smsMessagePartsReply(t, phone.conn, request, source, 200) {
			cancel()
		}
		if !labelled {
			// Historical unsplit implementations must finish their REAL worker
			// and expose the durable result, not hang waiting for absent parts.
			smsMessagePartsJoin(t, ctx, done)
			smsMessagePartsState(t, stores[1], saved, original, 200, token)
			return
		}
	}
	smsMessagePartsJoin(t, ctx, done)
	smsMessagePartsState(t, stores[1], saved, original, 200, token)
	smsMessagePartsNoReplay(t, ctx, manager, phone)
	if backend.count() != 0 {
		t.Error("MT application segmentation invoked the paid SMS backend")
	}
}

func TestSMSMessagePartsPartialRejectionOrCallerCancellationStaysUnknown(t *testing.T) {
	for _, cutoff := range []string{"reject-part-two", "cancel-before-part-two-final"} {
		t.Run(cutoff, func(t *testing.T) {
			ctx, stores, manager, backend, _ := smsMessagePartsFixture(t)
			phone := registerMessagePeer(t, manager.runtime, "1001")
			saved, err := stores[0].SaveSMSMessage(ctx, smsCaptureMessage("parts-partial-original", strings.Repeat("a", 600)))
			if err != nil {
				t.Fatal(err)
			}
			_, original := smsCaptureSavedInbox(t, stores[1], saved)
			expected := smsMessagePartsExpected(saved.Body)
			cancel, done := smsMessagePartsStart(ctx, manager)
			defer cancel()
			first, source, raw, err := smsMessagePartsReceive(phone.conn)
			if err != nil {
				t.Error(err)
				cancel()
				smsMessagePartsJoin(t, ctx, done)
				return
			}
			labelled := smsMessagePartsCheck(t, first, raw, saved, expected, 0)
			token := smsMessagePartsState(t, stores[1], saved, original, 504, "")
			if !labelled {
				status := 403
				if cutoff == "cancel-before-part-two-final" {
					status = 200
				}
				smsMessagePartsReply(t, phone.conn, first, source, status)
				smsMessagePartsJoin(t, ctx, done)
				cancel()
			} else {
				smsMessagePartsReply(t, phone.conn, first, source, 200)
				second, destination, secondRaw, readErr := smsMessagePartsReceive(phone.conn)
				if readErr != nil {
					t.Error(readErr)
					cancel()
					smsMessagePartsJoin(t, ctx, done)
					return
				}
				smsMessagePartsCheck(t, second, secondRaw, saved, expected, 1)
				smsMessagePartsState(t, stores[1], saved, original, 504, token)
				if cutoff == "reject-part-two" {
					smsMessagePartsReply(t, phone.conn, second, destination, 403)
				} else {
					cancel()
				}
				smsMessagePartsJoin(t, ctx, done)
			}
			smsMessagePartsState(t, stores[1], saved, original, 504, token)
			smsMessagePartsNoReplay(t, ctx, manager, phone)
			if backend.count() != 0 {
				t.Error("partial MT reached a paid SMS backend")
			}
		})
	}
}

func TestSMSMessagePartsActualInboxChangeBetweenPartsVetoesRemainingPayload(t *testing.T) {
	for _, mutation := range []string{"delete", "body"} {
		t.Run(mutation, func(t *testing.T) {
			ctx, stores, manager, backend, paused := smsMessagePartsFixture(t)
			phone := registerMessagePeer(t, manager.runtime, "1001")
			saved, err := stores[0].SaveSMSMessage(ctx, smsCaptureMessage("parts-actual-source-veto", strings.Repeat("b", 600)))
			if err != nil {
				t.Fatal(err)
			}
			_, original := smsCaptureSavedInbox(t, stores[1], saved)
			expected := smsMessagePartsExpected(saved.Body)
			cancel, done := smsMessagePartsStart(ctx, manager)
			defer cancel()
			first, source, raw, err := smsMessagePartsReceive(phone.conn)
			if err != nil {
				t.Error(err)
				cancel()
				smsMessagePartsJoin(t, ctx, done)
				return
			}
			labelled := smsMessagePartsCheck(t, first, raw, saved, expected, 0)
			token := smsMessagePartsState(t, stores[1], saved, original, 504, "")
			if !labelled {
				smsMessagePartsReply(t, phone.conn, first, source, 200)
				smsMessagePartsJoin(t, ctx, done)
				smsMessagePartsState(t, stores[1], saved, original, 504, token)
				return
			}
			gate := newSMSCASRelease()
			defer gate.close()
			paused.mu.Lock()
			paused.remaining, paused.reached, paused.release = 1, make(chan struct{}), gate.ch
			reached := paused.reached
			paused.mu.Unlock()
			smsMessagePartsReply(t, phone.conn, first, source, 200)
			select {
			case <-reached:
			case <-ctx.Done():
				t.Error("next actual part never reached the real awaited Store guard")
				cancel()
				gate.close()
				smsMessagePartsJoin(t, ctx, done)
				return
			}
			// Pause only the placement of a real Store read. The second SQLite
			// handle actually deletes/edits the Inbox before the next snapshot.
			if mutation == "delete" {
				err = stores[1].DeleteSMSMessage(ctx, saved.ID)
			} else {
				changed := saved
				changed.Body = "synthetic actual Inbox edited after accepted application part one"
				_, err = stores[1].SaveSMSMessage(ctx, changed)
			}
			if err != nil {
				t.Error(err)
				cancel()
			}
			gate.close()
			smsMessagePartsJoin(t, ctx, done)
			smsMessagePartsState(t, stores[1], saved, original, 504, token)
			smsMessagePartsNoReplay(t, ctx, manager, phone)
			if backend.count() != 0 {
				t.Error("MT source veto reached a paid SMS backend")
			}
		})
	}
}

func TestSMSMessagePartsContactsCannotCombineDifferentAcceptedParts(t *testing.T) {
	ctx, stores, manager, backend, _ := smsMessagePartsFixture(t)
	firstPhone, secondPhone := registerMessagePeer(t, manager.runtime, "1001"), registerMessagePeer(t, manager.runtime, "1001")
	saved, err := stores[0].SaveSMSMessage(ctx, smsCaptureMessage("parts-two-real-contact-prefixes", strings.Repeat("c", 600)))
	if err != nil {
		t.Fatal(err)
	}
	_, original := smsCaptureSavedInbox(t, stores[1], saved)
	expected := smsMessagePartsExpected(saved.Body)
	cancel, done := smsMessagePartsStart(ctx, manager)
	defer cancel()
	firstA, sourceA, rawA, errA := smsMessagePartsReceive(firstPhone.conn)
	firstB, sourceB, rawB, errB := smsMessagePartsReceive(secondPhone.conn)
	if errA != nil || errB != nil {
		t.Error("both real contacts must receive their own initial part:", errA, errB)
		cancel()
		smsMessagePartsJoin(t, ctx, done)
		return
	}
	labelledA := smsMessagePartsCheck(t, firstA, rawA, saved, expected, 0)
	labelledB := smsMessagePartsCheck(t, firstB, rawB, saved, expected, 0)
	token := smsMessagePartsState(t, stores[1], saved, original, 504, "")
	if !labelledA || !labelledB {
		smsMessagePartsReply(t, firstPhone.conn, firstA, sourceA, 200)
		smsMessagePartsReply(t, secondPhone.conn, firstB, sourceB, 403)
		smsMessagePartsJoin(t, ctx, done)
		smsMessagePartsState(t, stores[1], saved, original, 504, token)
		return
	}
	// A accepts only part 1. B accepts parts 1 and 2, but not part 3. Three
	// positive per-part responses across contacts are NOT one complete copy.
	smsMessagePartsReply(t, firstPhone.conn, firstA, sourceA, 200)
	smsMessagePartsReply(t, secondPhone.conn, firstB, sourceB, 200)
	secondA, destinationA, secondRawA, errA := smsMessagePartsReceive(firstPhone.conn)
	secondB, destinationB, secondRawB, errB := smsMessagePartsReceive(secondPhone.conn)
	if errA != nil || errB != nil {
		t.Error(errA, errB)
		cancel()
		smsMessagePartsJoin(t, ctx, done)
		return
	}
	smsMessagePartsCheck(t, secondA, secondRawA, saved, expected, 1)
	smsMessagePartsCheck(t, secondB, secondRawB, saved, expected, 1)
	smsMessagePartsReply(t, firstPhone.conn, secondA, destinationA, 403)
	smsMessagePartsReply(t, secondPhone.conn, secondB, destinationB, 200)
	thirdB, destination, thirdRaw, err := smsMessagePartsReceive(secondPhone.conn)
	if err != nil {
		t.Error(err)
		cancel()
		smsMessagePartsJoin(t, ctx, done)
		return
	}
	smsMessagePartsCheck(t, thirdB, thirdRaw, saved, expected, 2)
	smsMessagePartsState(t, stores[1], saved, original, 504, token)
	smsMessagePartsReply(t, secondPhone.conn, thirdB, destination, 403)
	smsMessagePartsJoin(t, ctx, done)
	smsMessagePartsState(t, stores[1], saved, original, 504, token)
	smsMessagePartsNoReplay(t, ctx, manager, firstPhone, secondPhone)
	if backend.count() != 0 {
		t.Error("multipart contact aggregation reached a paid SMS backend")
	}
}
