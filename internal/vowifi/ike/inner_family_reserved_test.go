package ike

import (
	"context"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

// RFC 7296 3.15.1 and 3.10 specify ignore-on-receipt semantics. These
// positive wire transcripts must not be turned into stricter rejection tests.
func TestInnerFamilyReservedConfigurationFieldsDoNotRejectValidAUTH(t *testing.T) {
	for _, mode := range []string{"attribute-bit", "header", "unknown-attribute"} {
		t.Run(mode, func(t *testing.T) {
			installer := &familyInstaller{}
			provider, _ := NewProvider(Config{Timeout: time.Second, Installer: installer})
			calls := 0
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				calls++
				tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, result: familyTestResult()}
				tr.finalMutation = func(p []payload) []payload {
					return familyReplace(p, payloadCP, func(v payload) payload {
						switch mode {
						case "attribute-bit":
							v.Body[4] |= 0x80
						case "header":
							for i := 1; i < 4; i++ {
								v.Body[i] = 0xff
							}
						case "unknown-attribute":
							v.Body = append(v.Body, 0xb0, 0x39, 0, 3, 1, 2, 3)
						}
						return v
					})
				}
				return tr, nil
			}
			session, err := provider.Start(context.Background(), familyRequest(&testAKAProvider{result: familyTestResult()}))
			if err != nil {
				t.Fatalf("reserved field rejected valid registration: %s: %v", mode, err)
			}
			defer session.Close(context.Background())
			if calls != 1 || installer.calls != 1 || installer.config.InnerLocalIPv4 == nil {
				t.Fatalf("reserved field changed plan: %s calls=%d installs=%d", mode, calls, installer.calls)
			}
		})
	}
}

func TestInnerFamilyEmptySPIProtocolIDIsIgnoredOnReceipt(t *testing.T) {
	for _, protocol := range []byte{0, 3, 255} {
		t.Run(string(rune('a'+int(protocol))), func(t *testing.T) {
			provider, _ := NewProvider(Config{Timeout: time.Second, Installer: unusedInstaller{}})
			calls := 0
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				calls++
				return &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, firstReply: func([]payload) []payload { return []payload{{Type: payloadNotify, Body: []byte{protocol, 0, 0, 38}}} }}, nil
			}
			aka := &testAKAProvider{}
			_, err := provider.Start(context.Background(), familyRequest(aka))
			if err == nil || calls != 3 || aka.calls != 0 || vowifi.EPDGFallbackAllowed(err) {
				t.Fatalf("empty SPI protocol field changed bounded plan: protocol=%d calls=%d AKA=%d err=%v", protocol, calls, aka.calls, err)
			}
		})
	}
}

func TestInnerFamilyReservedBitCannotHideMalformedKnownLength(t *testing.T) {
	installer := &familyInstaller{}
	provider, _ := NewProvider(Config{Timeout: time.Second, Installer: installer})
	calls := 0
	provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
		calls++
		tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, result: familyTestResult()}
		tr.finalMutation = func(p []payload) []payload {
			return familyReplace(p, payloadCP, func(payload) payload {
				return payload{Type: payloadCP, Body: []byte{2, 0, 0, 0, 0x80, 1, 0, 3, 10, 8, 2}}
			})
		}
		return tr, nil
	}
	_, err := provider.Start(context.Background(), familyRequest(&testAKAProvider{result: familyTestResult()}))
	if err == nil || calls != 1 || installer.calls != 0 || vowifi.EPDGFallbackAllowed(err) {
		t.Fatalf("reserved bit hid malformed length: calls=%d installs=%d err=%v", calls, installer.calls, err)
	}
}
