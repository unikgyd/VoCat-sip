package ike

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

// The initial RED transcript is retained unchanged through implementation.
// DH, encrypted IKE_AUTH, EAP MAC and final MSK AUTH are real; SIM/network and
// the CHILD_SA installer are synthetic and never touch an operator or interface.
type familyTranscript struct {
	*firstAuthCaptureTransport
	initial, response         []byte
	idi, idr, tsi, tsr        payload
	child                     []byte
	attributes                []uint16
	msk                       []byte
	result                    vowifi.AKAResult
	firstReply, finalMutation func([]payload) []payload
	deleteErr, closeErr       error
	deletes, closes           int
}

func (tr *familyTranscript) RoundTrip(ctx context.Context, packet []byte) ([]byte, error) {
	replies, err := tr.RoundTripExchange(ctx, [][]byte{packet})
	if err != nil {
		return nil, err
	}
	return replies[0], nil
}

func (tr *familyTranscript) RoundTripExchange(ctx context.Context, packets [][]byte) ([][]byte, error) {
	if len(packets) != 1 {
		return nil, errors.New("test: unexpected fragmentation")
	}
	packet := packets[0]
	h, _, err := parseIKEPacket(packet)
	if err != nil {
		return nil, err
	}
	if h.Exchange == exchangeIKEInit {
		tr.initial = append([]byte(nil), packet...)
		response, err := tr.answerIKEInit(packet)
		tr.response = append([]byte(nil), response...)
		return [][]byte{response}, err
	}
	h, items, err := decryptPayloads(packet, tr.suite, tr.keys.SKei, tr.keys.SKai)
	if err != nil {
		return nil, err
	}
	var reply []payload
	switch h.MessageID {
	case 1:
		tr.idi, err = onePayload(items, payloadIDi)
		if err != nil {
			return nil, err
		}
		tr.tsi, err = onePayload(items, payloadTSi)
		if err != nil {
			return nil, err
		}
		tr.tsr, err = onePayload(items, payloadTSr)
		if err != nil {
			return nil, err
		}
		sa, err := onePayload(items, payloadSA)
		if err != nil {
			return nil, err
		}
		tr.child = append([]byte(nil), sa.Body...)
		cp, err := onePayload(items, payloadCP)
		if err != nil {
			return nil, err
		}
		for offset := 4; offset < len(cp.Body); offset += 4 {
			tr.attributes = append(tr.attributes, binary.BigEndian.Uint16(cp.Body[offset:offset+2]))
		}
		tr.idr = payload{Type: payloadIDr, Body: append([]byte{2, 0, 0, 0}, []byte("epdg.example")...)}
		if tr.firstReply != nil {
			reply = tr.firstReply(nil)
			break
		}
		keys, err := deriveAKAKeys(tr.idi.Body[4:], tr.result.IK, tr.result.CK)
		if err != nil {
			return nil, err
		}
		tr.msk = keys.MSK
		randAttr, _ := marshalAKAAttribute(akaAttrRAND, append([]byte{0, 0}, bytes.Repeat([]byte{0xa1}, 16)...))
		autnAttr, _ := marshalAKAAttribute(akaAttrAUTN, append([]byte{0, 0}, bytes.Repeat([]byte{0xa2}, 16)...))
		macAttr, _ := marshalAKAAttribute(akaAttrMAC, make([]byte, 18))
		data := append([]byte{akaSubtypeChallenge, 0, 0}, randAttr...)
		data = append(data, autnAttr...)
		data = append(data, macAttr...)
		eap, _ := marshalEAPPacket(eapPacket{Code: eapRequest, Identifier: 21, Type: eapTypeAKA, Data: data})
		copy(eap[len(eap)-16:], akaMAC(keys.KAut, eap))
		reply = []payload{tr.idr, {Type: payloadEAP, Body: eap}}
	case 2:
		eap, err := onePayload(items, payloadEAP)
		if err != nil {
			return nil, err
		}
		parsed, err := parseEAPPacket(eap.Body)
		if err != nil || parsed.Code != eapResponse || len(parsed.Data) < 3 || parsed.Data[0] != akaSubtypeChallenge {
			return nil, errors.New("test: expected authenticated AKA response")
		}
		success, _ := marshalEAPPacket(eapPacket{Code: eapSuccess, Identifier: 22})
		reply = []payload{{Type: payloadEAP, Body: success}}
	case 3:
		signed, err := responderSignedOctets(tr.response, tr.nonceI, tr.suite, tr.keys.SKpr, tr.idr)
		if err != nil {
			return nil, err
		}
		padded, _ := prf(tr.suite, tr.msk, []byte("Key Pad for IKEv2"))
		mic, _ := prf(tr.suite, padded, signed)
		auth := payload{Type: payloadAuth, Body: append([]byte{authMethodSharedKeyMIC, 0, 0, 0}, mic...)}
		proposals, err := parseProposals(tr.child)
		if err != nil {
			return nil, err
		}
		proposals[0].SPI = []byte{7, 6, 5, 4}
		selected := []transform{}
		seen := map[uint8]bool{}
		for _, candidate := range proposals[0].Transforms {
			if !seen[candidate.Type] {
				selected = append(selected, candidate)
				seen[candidate.Type] = true
			}
		}
		proposals[0].Transforms = selected
		sa, _ := marshalProposals(proposals)
		// A single-family reply follows the actual requested CP attributes.
		v6 := !containsFamilyAttribute(tr.attributes, configInternalIPv4Address)
		cp := familyReplyConfiguration(v6, v6)
		reply = []payload{auth, {Type: payloadSA, Body: sa}, tr.tsi, tr.tsr, cp}
		if tr.finalMutation != nil {
			reply = tr.finalMutation(reply)
		}
	default:
		return nil, errors.New("test: unexpected IKE_AUTH message id")
	}
	response, err := encryptPayloads(ikeHeader{InitiatorSPI: tr.spii, ResponderSPI: tr.spir, Exchange: exchangeIKEAuth, Flags: flagResponse, MessageID: h.MessageID}, reply, tr.suite, tr.keys.SKer, tr.keys.SKar, nil)
	return [][]byte{response}, err
}
func (tr *familyTranscript) SendSessionPacket(context.Context, []byte, bool) error {
	tr.deletes++
	return tr.deleteErr
}
func (tr *familyTranscript) ReceiveSessionPacket(ctx context.Context, _ []byte) (int, bool, error) {
	<-ctx.Done()
	return 0, false, ctx.Err()
}
func (tr *familyTranscript) Close() error { tr.closes++; return tr.closeErr }
func containsFamilyAttribute(values []uint16, want uint16) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func familyReplyConfiguration(localV6, pcscfV6 bool) payload {
	body := []byte{configReply, 0, 0, 0}
	add := func(kind uint16, value []byte) {
		header := make([]byte, 4)
		binary.BigEndian.PutUint16(header, kind)
		binary.BigEndian.PutUint16(header[2:], uint16(len(value)))
		body = append(body, header...)
		body = append(body, value...)
	}
	if localV6 {
		add(configInternalIPv6Address, append(net.ParseIP("2001:db8::10").To16(), 64))
	} else {
		add(configInternalIPv4Address, net.ParseIP("10.8.0.2").To4())
	}
	if pcscfV6 {
		add(configPCSCFIPv6Address, net.ParseIP("2001:db8::20").To16())
	} else {
		add(configPCSCFIPv4Address, net.ParseIP("10.8.0.1").To4())
	}
	return payload{Type: payloadCP, Body: body}
}

type familyInstalled struct{}

func (familyInstalled) Close(context.Context) error { return nil }

type familyInstaller struct {
	calls  int
	config ChildSAConfig
}

func (i *familyInstaller) Install(_ context.Context, c ChildSAConfig) (ChildSAHandle, error) {
	i.calls++
	i.config = c
	return familyInstalled{}, nil
}
func familyTestResult() vowifi.AKAResult {
	return vowifi.AKAResult{RES: bytes.Repeat([]byte{0x91}, 8), CK: bytes.Repeat([]byte{0x92}, 16), IK: bytes.Repeat([]byte{0x93}, 16)}
}
func familyRequest(aka vowifi.AKAProvider) vowifi.TunnelRequest {
	return vowifi.TunnelRequest{DeviceID: "family-test", EPDG: "epdg.example", Identity: vowifi.SIMIdentity{ICCID: "8944100000000000001", IMSI: "234150123456789", HomeMCC: "234", HomeMNC: "15"}, AKA: aka}
}

func TestInnerFamilyPreAKARejectionChangesTheActualCPAndSelectors(t *testing.T) {
	provider, _ := NewProvider(Config{Timeout: time.Second, Installer: unusedInstaller{}})
	var attempts []*familyTranscript
	provider.transportFactory = func(_ context.Context, _ transportConfig, _ vowifi.ProxyRoute, _ string) (datagramTransport, error) {
		tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, firstReply: func([]payload) []payload { return []payload{makeNotify(38, nil)} }}
		attempts = append(attempts, tr)
		if len(attempts) > 3 {
			return nil, errors.New("test: retry budget exceeded")
		}
		return tr, nil
	}
	aka := &testAKAProvider{}
	_, err := provider.Start(context.Background(), familyRequest(aka))
	if err == nil || len(attempts) != 3 {
		t.Fatalf("family attempts=%d, error=%v; want bounded dual/v6/v4", len(attempts), err)
	}
	want := [][]uint16{{1, 8, 3, 10, 20, 21, 7}, {8, 10, 21, 7}, {1, 3, 20, 7}}
	for i, tr := range attempts {
		if !reflect.DeepEqual(tr.attributes, want[i]) {
			t.Fatalf("attempt %d CP=%v want %v", i, tr.attributes, want[i])
		}
		selectors, _ := parseTrafficSelectors(tr.tsi)
		wantCount := 1
		if i == 0 {
			wantCount = 2
		}
		if len(selectors) != wantCount || tr.closes != 1 || tr.deletes != 1 {
			t.Fatalf("attempt %d selectors=%v closes=%d deletes=%d", i, selectors, tr.closes, tr.deletes)
		}
	}
	if aka.calls != 0 || vowifi.EPDGFallbackAllowed(err) {
		t.Fatalf("family failure became identity retry: aka=%d err=%v", aka.calls, err)
	}
}

func TestInnerFamilyVerifiedFinalAUTHMismatchRebuildsBeforeInstalling(t *testing.T) {
	installer := &familyInstaller{}
	provider, _ := NewProvider(Config{Timeout: time.Second, Installer: installer})
	var attempts []*familyTranscript
	provider.transportFactory = func(_ context.Context, _ transportConfig, _ vowifi.ProxyRoute, _ string) (datagramTransport, error) {
		tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, result: familyTestResult()}
		if len(attempts) == 0 {
			tr.finalMutation = func(items []payload) []payload {
				for i := range items {
					if items[i].Type == payloadCP {
						items[i] = familyReplyConfiguration(false, true)
					}
				}
				return items
			}
		}
		attempts = append(attempts, tr)
		if len(attempts) > 2 {
			return nil, errors.New("test: unwanted retry")
		}
		return tr, nil
	}
	aka := &testAKAProvider{result: familyTestResult()}
	session, err := provider.Start(context.Background(), familyRequest(aka))
	if err != nil {
		t.Fatalf("verified final CP mismatch did not recover: attempts=%d error=%v", len(attempts), err)
	}
	defer session.Close(context.Background())
	if len(attempts) != 2 || aka.calls != 2 || installer.calls != 1 || installer.config.InnerLocalIPv6 == nil || installer.config.InnerLocalIPv4 != nil {
		t.Fatalf("attempts=%d AKA=%d installs=%d network=%+v", len(attempts), aka.calls, installer.calls, installer.config)
	}
	if attempts[0].closes != 1 || attempts[0].deletes != 1 {
		t.Fatal("replacement started without tearing down the rejected IKE SA")
	}
	if bytes.Equal(attempts[0].initial, attempts[1].initial) {
		t.Fatal("replacement reused the failed IKE SA transcript")
	}
}
