package ike

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"vocat/internal/vowifi"
)

type cookieReceiptCase struct {
	Name     string
	Length   int
	Protocol byte
	SPI      int `json:"spi_size"`
	Extra    string
	Valid    bool
}

func cookieReceiptCases(t *testing.T) []cookieReceiptCase {
	t.Helper()
	raw, err := os.ReadFile("../../../../contracts/ike-init-cookie-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []cookieReceiptCase
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Cases
}

func (c cookieReceiptCase) payloads() []payload {
	body := append([]byte{c.Protocol, byte(c.SPI), 0x40, 6}, bytes.Repeat([]byte{7}, c.SPI+c.Length)...)
	items := []payload{{Type: payloadNotify, Body: body}}
	switch c.Extra {
	case "duplicate":
		items = append(items, items[0])
	case "sa":
		items = append(items, payload{Type: payloadSA})
	case "ke":
		items = append(items, payload{Type: payloadKE})
	case "nonce":
		items = append(items, payload{Type: payloadNonce})
	case "fatal":
		items = append(items, makeNotify(24, nil))
	case "malformed":
		items = append(items, payload{Type: payloadNotify, Body: []byte{0}})
	}
	return items
}

func TestCookieParitySharedReceiptVectors(t *testing.T) {
	for _, c := range cookieReceiptCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			cookie, found, err := ikeInitCookie(c.payloads())
			if (err == nil && found && len(cookie) == c.Length) != c.Valid {
				t.Fatalf("valid=%v found=%v length=%d err=%v", c.Valid, found, len(cookie), err)
			}
		})
	}
}

type cookieParityTransport struct {
	*firstAuthCaptureTransport
	spi      [8]byte
	protocol byte
	original []payload
	closes   int
	invalid  []payload
	rotate   bool
}

func (tr *cookieParityTransport) RoundTrip(ctx context.Context, packet []byte) ([]byte, error) {
	h, body, err := parseIKEPacket(packet)
	if err != nil {
		return nil, err
	}
	items, err := parsePayloadChain(h.NextPayload, body)
	if err != nil {
		return nil, err
	}
	if tr.calls == 0 {
		tr.original = items
	} else {
		if h.ResponderSPI != [8]byte{} || h.MessageID != 0 || !reflect.DeepEqual(items[1:], tr.original) {
			tr.t.Fatal("COOKIE refresh changed the original transcript")
		}
	}
	if tr.rotate {
		tr.calls++
		first, body, err := marshalPayloadChain([]payload{makeNotify(notifyCookie, []byte{byte(tr.calls)})})
		return (ikeHeader{InitiatorSPI: h.InitiatorSPI, ResponderSPI: tr.spi, Exchange: exchangeIKEInit, Flags: flagResponse, NextPayload: first}).marshal(body), err
	}
	result, err := tr.firstAuthCaptureTransport.RoundTrip(ctx, packet)
	if err != nil || tr.calls != 1 {
		return result, err
	}
	response, responseBody, err := parseIKEPacket(result)
	if err != nil {
		return nil, err
	}
	reply, err := parsePayloadChain(response.NextPayload, responseBody)
	if err != nil {
		return nil, err
	}
	reply[0].Body[0] = tr.protocol
	if tr.invalid != nil {
		reply = tr.invalid
	}
	response.ResponderSPI = tr.spi
	response.NextPayload, responseBody, err = marshalPayloadChain(reply)
	return response.marshal(responseBody), err
}
func (tr *cookieParityTransport) Close() error { tr.closes++; return nil }

func TestCookieParityActualProviderAcceptsNonzeroSPIAndIgnoredProtocol(t *testing.T) {
	for _, protocol := range []byte{0, 255} {
		t.Run(fmt.Sprint(protocol), func(t *testing.T) {
			provider, _ := NewProvider(Config{Installer: unusedInstaller{}})
			tr := &cookieParityTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t, wantEAPOnly: true, cookieChallenge: []byte{1, 2, 3}}, spi: [8]byte{1}, protocol: protocol}
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				return tr, nil
			}
			aka := &testAKAProvider{}
			_, err := provider.Start(context.Background(), familyRequest(aka))
			if !errors.Is(err, errFirstAuthObserved) || tr.calls != 3 || !tr.cookieSeen || tr.closes != 1 || aka.calls != 0 {
				t.Fatalf("calls=%d closes=%d AKA=%d err=%v", tr.calls, tr.closes, aka.calls, err)
			}
		})
	}
}

func TestCookieParityActualProviderRejectsInvalidCookieBeforeAnotherInit(t *testing.T) {
	for _, c := range cookieReceiptCases(t) {
		if c.Valid {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			provider, _ := NewProvider(Config{Installer: unusedInstaller{}})
			tr := &cookieParityTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t, cookieChallenge: bytes.Repeat([]byte{7}, c.Length)}, invalid: c.payloads()}
			// Ensure the underlying fixture always emits a first COOKIE even for the empty vector.
			tr.cookieChallenge = []byte{1}
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				return tr, nil
			}
			aka := &testAKAProvider{}
			_, err := provider.Start(context.Background(), familyRequest(aka))
			if err == nil || tr.calls != 1 || tr.closes != 1 || aka.calls != 0 {
				t.Fatalf("calls=%d closes=%d AKA=%d err=%v", tr.calls, tr.closes, aka.calls, err)
			}
		})
	}
}

func TestCookieParityActualProviderBoundsRefresh(t *testing.T) {
	provider, _ := NewProvider(Config{Installer: unusedInstaller{}})
	tr := &cookieParityTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, rotate: true, spi: [8]byte{1}}
	provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
		return tr, nil
	}
	aka := &testAKAProvider{}
	_, err := provider.Start(context.Background(), familyRequest(aka))
	if err == nil || tr.calls != 4 || tr.closes != 1 || aka.calls != 0 {
		t.Fatalf("calls=%d closes=%d AKA=%d err=%v", tr.calls, tr.closes, aka.calls, err)
	}
}
