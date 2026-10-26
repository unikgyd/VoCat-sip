package ike

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

type profileWireAKA struct {
	testAKAProvider
	physical vowifi.SIMIdentity
}

func TestProfileIdentityPreChallengeRejectionAllowsBoundedCandidateFallback(t *testing.T) {
	provider, _ := NewProvider(Config{Timeout: time.Second, Installer: unusedInstaller{}})
	transport := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, firstReply: func([]payload) []payload { return []payload{{Type: payloadEAP, Body: []byte{eapFailure, 1, 0, 4}}} }}
	provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
		return transport, nil
	}
	aka := &testAKAProvider{}
	_, err := provider.Start(context.Background(), familyRequest(aka))
	if err == nil || !vowifi.EPDGFallbackAllowed(err) || aka.calls != 0 || transport.deletes != 1 || transport.closes != 1 {
		t.Fatalf("pre-challenge rejection was not safely retryable: AKA=%d delete=%d close=%d err=%v", aka.calls, transport.deletes, transport.closes, err)
	}
}

func (p *profileWireAKA) Authenticate(ctx context.Context, id vowifi.SIMIdentity, c vowifi.AKAChallenge) (vowifi.AKAResult, error) {
	p.physical = id
	return p.testAKAProvider.Authenticate(ctx, id, c)
}

func TestProfileIdentityEAPUsesSelectedNAIButPhysicalAKABinding(t *testing.T) {
	live := vowifi.SIMIdentity{ICCID: "8944100000000000001", IMSI: "234150123456789", HomeMCC: "234", HomeMNC: "15"}
	selected := live
	selected.IMSI = "204040123456789"
	selected.HomeMCC = "204"
	selected.HomeMNC = "04"
	bound, err := vowifi.ProfileAuthenticationIdentity(live, selected)
	if err != nil {
		t.Fatal(err)
	}
	aka := &profileWireAKA{testAKAProvider: testAKAProvider{result: vowifi.AKAResult{RES: bytes.Repeat([]byte{1}, 8), CK: bytes.Repeat([]byte{2}, 16), IK: bytes.Repeat([]byte{3}, 16)}}}
	client, err := newAKAClient(bound, aka)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(client.identity), selected.IMSI) || strings.Contains(string(client.identity), live.IMSI) {
		t.Fatal("wire identity did not use selected subscriber")
	}
	keys, err := deriveAKAKeys(client.identity, aka.result.IK, aka.result.CK)
	if err != nil {
		t.Fatal(err)
	}
	rand, _ := marshalAKAAttribute(akaAttrRAND, append([]byte{0, 0}, bytes.Repeat([]byte{4}, 16)...))
	autn, _ := marshalAKAAttribute(akaAttrAUTN, append([]byte{0, 0}, bytes.Repeat([]byte{5}, 16)...))
	mac, _ := marshalAKAAttribute(akaAttrMAC, make([]byte, 18))
	data := append([]byte{akaSubtypeChallenge, 0, 0}, rand...)
	data = append(data, autn...)
	data = append(data, mac...)
	packet, _ := marshalEAPPacket(eapPacket{Code: eapRequest, Identifier: 9, Type: eapTypeAKA, Data: data})
	copy(packet[len(packet)-16:], akaMAC(keys.KAut, packet))
	action, err := client.handle(context.Background(), packet)
	if err != nil || len(action.Response) == 0 || aka.calls != 1 || aka.physical.IMSI != live.IMSI || aka.physical.ICCID != live.ICCID {
		t.Fatalf("AKA binding mismatch: calls=%d err=%v", aka.calls, err)
	}
	bound.IMSI = "204040123456788"
	if _, err := newAKAClient(bound, aka); err == nil || aka.calls != 1 {
		t.Fatal("mutated identity reused physical source")
	}
}

// Terminate the next encrypted EAP round, including after a client-error
// response. This exercises the producer of fallback proof, not a mocked error.
type profileAfterChallengeTransport struct{ *familyTranscript }

func (tr *profileAfterChallengeTransport) RoundTripExchange(ctx context.Context, packets [][]byte) ([][]byte, error) {
	if len(packets) == 1 {
		h, _, err := parseIKEPacket(packets[0])
		if err == nil && h.Exchange == exchangeIKEAuth && h.MessageID == 2 {
			if _, _, err := decryptPayloads(packets[0], tr.suite, tr.keys.SKei, tr.keys.SKai); err != nil {
				return nil, err
			}
			response, err := encryptPayloads(ikeHeader{InitiatorSPI: tr.spii, ResponderSPI: tr.spir, Exchange: exchangeIKEAuth, Flags: flagResponse, MessageID: 2}, []payload{{Type: payloadEAP, Body: []byte{eapFailure, 21, 0, 4}}}, tr.suite, tr.keys.SKer, tr.keys.SKar, nil)
			return [][]byte{response}, err
		}
	}
	return tr.familyTranscript.RoundTripExchange(ctx, packets)
}

func TestProfileIdentityRejectionProofCannotHideChallengeBackoffOrCleanup(t *testing.T) {
	for _, mode := range []string{"challenge", "missing-challenge-attributes", "challenge-reserved", "challenge-truncated", "malformed-attributes", "invalid-identity-request", "backoff", "delete-error", "close-error", "mixed-auth"} {
		t.Run(mode, func(t *testing.T) {
			provider, _ := NewProvider(Config{Timeout: time.Second, Installer: unusedInstaller{}})
			tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, result: familyTestResult()}
			if mode != "challenge" {
				tr.firstReply = func([]payload) []payload {
					body := []byte{eapFailure, 1, 0, 4}
					var data []byte
					switch mode {
					case "missing-challenge-attributes":
						data = []byte{akaSubtypeChallenge, 0, 0}
					case "challenge-reserved":
						data = []byte{akaSubtypeChallenge, 1, 0}
					case "challenge-truncated":
						data = []byte{akaSubtypeChallenge}
					case "malformed-attributes":
						data = []byte{akaSubtypeChallenge, 0, 0, akaAttrRAND, 0}
					case "invalid-identity-request":
						data = []byte{akaSubtypeIdentity, 0, 0}
					}
					if data != nil {
						body, _ = marshalEAPPacket(eapPacket{Code: eapRequest, Identifier: 21, Type: eapTypeAKA, Data: data})
					}
					reply := []payload{{Type: payloadEAP, Body: body}}
					if mode == "backoff" {
						reply = append(reply, makeNotify(41041, []byte{1}))
					}
					if mode == "mixed-auth" {
						reply = append(reply, makeNotify(notifyAuthenticationFailed, nil))
					}
					return reply
				}
			}
			if mode == "delete-error" {
				tr.deleteErr = errors.New("synthetic DELETE failure")
			}
			if mode == "close-error" {
				tr.closeErr = errors.New("synthetic close failure")
			}
			calls := 0
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				calls++
				return &profileAfterChallengeTransport{tr}, nil
			}
			aka := &testAKAProvider{result: familyTestResult()}
			_, err := provider.Start(context.Background(), familyRequest(aka))
			wantAKA := 0
			if mode == "challenge" {
				wantAKA = 1
			}
			if err == nil || vowifi.EPDGFallbackAllowed(err) || calls != 1 || aka.calls != wantAKA || tr.deletes != 1 || tr.closes != 1 {
				t.Fatalf("unsafe proof: attempts=%d AKA=%d deletes=%d closes=%d err=%v", calls, aka.calls, tr.deletes, tr.closes, err)
			}
		})
	}
}

func TestProfileIdentityFamilyHistoryMustRemainPreChallenge(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(strconv.FormatBool(authenticated), func(t *testing.T) {
			provider, _ := NewProvider(Config{Timeout: time.Second, Installer: unusedInstaller{}})
			attempts := 0
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				attempts++
				tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, result: familyTestResult()}
				if attempts == 1 && authenticated {
					tr.finalMutation = func(p []payload) []payload {
						return familyReplace(p, payloadCP, func(payload) payload { return familyReplyConfiguration(false, true) })
					}
				} else if attempts == 1 {
					tr.firstReply = func([]payload) []payload { return []payload{makeNotify(36, nil)} }
				} else {
					tr.firstReply = func([]payload) []payload { return []payload{{Type: payloadEAP, Body: []byte{eapFailure, 1, 0, 4}}} }
				}
				return tr, nil
			}
			aka := &testAKAProvider{result: familyTestResult()}
			_, err := provider.Start(context.Background(), familyRequest(aka))
			wantAKA := 0
			if authenticated {
				wantAKA = 1
			}
			if err == nil || vowifi.EPDGFallbackAllowed(err) == authenticated || attempts != 2 || aka.calls != wantAKA {
				t.Fatalf("family history lost: authenticated=%v attempts=%d AKA=%d err=%v", authenticated, attempts, aka.calls, err)
			}
		})
	}
}

func TestProfileIdentityValidIdentityOnlyRoundsStillAllowFallback(t *testing.T) {
	for _, kind := range []uint8{eapTypeIdentity, eapTypeAKA} {
		t.Run(strconv.Itoa(int(kind)), func(t *testing.T) {
			provider, _ := NewProvider(Config{Timeout: time.Second, Installer: unusedInstaller{}})
			tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}}
			tr.firstReply = func([]payload) []payload {
				var data []byte
				if kind == eapTypeAKA {
					attr, _ := marshalAKAAttribute(akaAttrFullAuthIDReq, []byte{0, 0})
					data = append([]byte{akaSubtypeIdentity, 0, 0}, attr...)
				}
				body, _ := marshalEAPPacket(eapPacket{Code: eapRequest, Identifier: 21, Type: kind, Data: data})
				return []payload{{Type: payloadEAP, Body: body}}
			}
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				return &profileAfterChallengeTransport{tr}, nil
			}
			aka := &testAKAProvider{}
			_, err := provider.Start(context.Background(), familyRequest(aka))
			if err == nil || !vowifi.EPDGFallbackAllowed(err) || aka.calls != 0 || tr.closes != 1 || tr.deletes != 1 {
				t.Fatalf("valid identity round disabled bounded fallback: AKA=%d err=%v", aka.calls, err)
			}
		})
	}
}

func TestProfileIdentityEarlierBackoffCannotBeForgotten(t *testing.T) {
	provider, _ := NewProvider(Config{Timeout: time.Second, Installer: unusedInstaller{}})
	tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}}
	tr.firstReply = func([]payload) []payload {
		body, _ := marshalEAPPacket(eapPacket{Code: eapRequest, Identifier: 21, Type: eapTypeIdentity})
		return []payload{{Type: payloadEAP, Body: body}, makeNotify(41041, []byte{1})}
	}
	provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
		return &profileAfterChallengeTransport{tr}, nil
	}
	aka := &testAKAProvider{}
	_, err := provider.Start(context.Background(), familyRequest(aka))
	if err == nil || vowifi.EPDGFallbackAllowed(err) || aka.calls != 0 || tr.deletes != 1 || tr.closes != 1 {
		t.Fatalf("earlier backoff did not fence identity fallback: AKA=%d err=%v", aka.calls, err)
	}
}
