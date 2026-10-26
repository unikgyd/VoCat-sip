package ike

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

func familyReplace(items []payload, kind uint8, change func(payload) payload) []payload {
	for i := range items {
		if items[i].Type == kind {
			items[i] = change(items[i])
		}
	}
	return items
}

func TestInnerFamilyDoesNotRetryMixedOrMalformedInitialRejection(t *testing.T) {
	cases := map[string][]payload{
		"authentication-after":  {makeNotify(36, nil), makeNotify(24, nil)},
		"authentication-before": {makeNotify(24, nil), makeNotify(38, nil)},
		"backoff":               {makeNotify(36, nil), makeNotify(41041, []byte{1})},
		"proposal":              {makeNotify(38, nil), makeNotify(notifyNoProposal, nil)},
		"malformed-notify":      {makeNotify(36, nil), {Type: payloadNotify, Body: []byte{0}}},
		"unexpected-data":       {makeNotify(36, []byte{1})},
		"nonempty-spi":          {{Type: payloadNotify, Body: []byte{protocolESP, 4, 0, 36, 0, 0, 0, 1}}},
		"eap-present":           {makeNotify(36, nil), {Type: payloadEAP, Body: []byte{eapFailure, 1, 0, 4}}},
		"invalid-idr":           {makeNotify(36, nil), {Type: payloadIDr, Body: []byte{2}}},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			provider, _ := NewProvider(Config{Timeout: time.Second, Installer: unusedInstaller{}, AutoProposalFallback: true})
			calls := 0
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				calls++
				return &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, firstReply: func([]payload) []payload { return reply }}, nil
			}
			_, err := provider.Start(context.Background(), familyRequest(&testAKAProvider{}))
			if err == nil || calls != 1 || vowifi.EPDGFallbackAllowed(err) {
				t.Fatalf("unsafe retry: calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestInnerFamilyFinalSecurityAndParsingErrorsAreFatal(t *testing.T) {
	cases := map[string]func([]payload) []payload{
		"invalid-auth": func(p []payload) []payload {
			p = familyReplace(p, payloadCP, func(payload) payload { return familyReplyConfiguration(false, true) })
			return familyReplace(p, payloadAuth, func(v payload) payload { v.Body[len(v.Body)-1] ^= 1; return v })
		},
		"missing-auth": func(p []payload) []payload { return p[1:] },
		"malformed-cp": func(p []payload) []payload {
			return familyReplace(p, payloadCP, func(payload) payload { return payload{Type: payloadCP, Body: []byte{2, 0, 0, 0, 0, 1, 0, 3, 10, 8, 1}} })
		},
		"invalid-local": func(p []payload) []payload {
			return familyReplace(p, payloadCP, func(v payload) payload { copy(v.Body[8:12], []byte{127, 0, 0, 1}); return v })
		},
		"empty-selectors": func(p []payload) []payload {
			return familyReplace(p, payloadTSi, func(payload) payload { return payload{Type: payloadTSi, Body: []byte{0, 0, 0, 0}} })
		},
		"unoffered-esp": func(p []payload) []payload {
			return familyReplace(p, payloadSA, func(v payload) payload { v.Body[4] = 2; return v })
		},
		"fatal-notify": func(p []payload) []payload { return append(p, makeNotify(24, nil), makeNotify(36, nil)) },
		"duplicate-cp": func(p []payload) []payload { return append(p, familyReplyConfiguration(false, true)) },
	}
	for name, mutation := range cases {
		t.Run(name, func(t *testing.T) {
			installer := &familyInstaller{}
			provider, _ := NewProvider(Config{Timeout: time.Second, Installer: installer, AutoProposalFallback: true})
			calls := 0
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				calls++
				return &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, result: familyTestResult(), finalMutation: mutation}, nil
			}
			aka := &testAKAProvider{result: familyTestResult()}
			_, err := provider.Start(context.Background(), familyRequest(aka))
			if err == nil || calls != 1 || installer.calls != 0 || aka.calls != 1 || vowifi.EPDGFallbackAllowed(err) {
				t.Fatalf("unsafe final recovery: attempts=%d installs=%d AKA=%d error=%v", calls, installer.calls, aka.calls, err)
			}
			if name == "invalid-auth" && !strings.Contains(err.Error(), "final responder AUTH is invalid") {
				t.Fatalf("invalid AUTH was hidden: %v", err)
			}
		})
	}
}

type familyCancelTransport struct {
	*familyTranscript
	cancel context.CancelFunc
}

func (tr *familyCancelTransport) Close() error { tr.cancel(); return tr.familyTranscript.Close() }

func TestInnerFamilyCleanupOrCancellationCannotAuthorizeReplacement(t *testing.T) {
	for _, mode := range []string{"delete", "transport-close", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			provider, _ := NewProvider(Config{Timeout: time.Second, Installer: unusedInstaller{}})
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				calls++
				tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, firstReply: func([]payload) []payload { return []payload{makeNotify(36, nil)} }}
				switch mode {
				case "delete":
					tr.deleteErr = errors.New("synthetic DELETE send failure")
				case "transport-close":
					tr.closeErr = errors.New("synthetic close failure")
				case "cancel":
					return &familyCancelTransport{tr, cancel}, nil
				}
				return tr, nil
			}
			_, err := provider.Start(ctx, familyRequest(&testAKAProvider{}))
			if err == nil || calls != 1 || vowifi.EPDGFallbackAllowed(err) {
				t.Fatalf("unsafe cleanup recovery: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestInnerFamilyKeepsEndpointProxyHomeAndExclusions(t *testing.T) {
	provider, _ := NewProvider(Config{Timeout: time.Second, Installer: unusedInstaller{}})
	var hosts []string
	var identities []string
	var previous *familyTranscript
	request := familyRequest(&testAKAProvider{})
	request.Proxy = vowifi.ProxyRoute{Mode: vowifi.ProxyModeSOCKS5}
	request.ExcludedEPDGAddresses = []string{"203.0.113.9"}
	provider.transportFactory = func(_ context.Context, c transportConfig, proxy vowifi.ProxyRoute, host string) (datagramTransport, error) {
		if previous != nil && (previous.closes != 1 || previous.deletes != 1) {
			t.Fatal("new transport before old teardown")
		}
		if !reflect.DeepEqual(proxy, request.Proxy) || !reflect.DeepEqual(c.ExcludedAddresses, request.ExcludedEPDGAddresses) {
			t.Fatal("family retry changed routing")
		}
		c.ExcludedAddresses[0] = "203.0.113.99"
		hosts = append(hosts, host)
		tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}}
		tr.firstReply = func([]payload) []payload {
			identities = append(identities, string(tr.idi.Body))
			return []payload{makeNotify(36, nil)}
		}
		previous = tr
		return tr, nil
	}
	_, err := provider.Start(context.Background(), request)
	if err == nil || !reflect.DeepEqual(hosts, []string{"epdg.example", "198.51.100.20", "198.51.100.20"}) || len(identities) != 3 || identities[0] != identities[1] || identities[1] != identities[2] || request.ExcludedEPDGAddresses[0] != "203.0.113.9" {
		t.Fatalf("routing changed: hosts=%v identities=%v err=%v", hosts, identities, err)
	}
}

func TestInnerFamilyRejectsWidenedSingleFamilyAndUnrequestedCP(t *testing.T) {
	for _, mode := range []string{"selectors", "cp"} {
		t.Run(mode, func(t *testing.T) {
			provider, _ := NewProvider(Config{Timeout: time.Second, Installer: unusedInstaller{}})
			calls := 0
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				calls++
				tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, result: familyTestResult()}
				if calls == 1 {
					tr.firstReply = func([]payload) []payload { return []payload{makeNotify(38, nil)} }
				} else {
					tr.finalMutation = func(p []payload) []payload {
						if mode == "selectors" {
							return familyReplace(p, payloadTSr, func(payload) payload { return dualStackTrafficSelectors(payloadTSr) })
						}
						return familyReplace(p, payloadCP, func(payload) payload { return familyReplyConfiguration(false, false) })
					}
				}
				return tr, nil
			}
			_, err := provider.Start(context.Background(), familyRequest(&testAKAProvider{result: familyTestResult()}))
			if err == nil || calls != 2 || vowifi.EPDGFallbackAllowed(err) {
				t.Fatalf("unoffered family accepted: attempts=%d error=%v", calls, err)
			}
		})
	}
}

func TestInnerFamilyParserCannotTurnTruncatedAttributesIntoMissingAddresses(t *testing.T) {
	for _, kind := range []uint16{1, 3, 8, 10, 20, 21} {
		t.Run(string(rune('a'+kind)), func(t *testing.T) {
			_, err := parseConfiguration(payload{Type: payloadCP, Body: []byte{2, 0, 0, 0, byte(kind >> 8), byte(kind), 0, 1, 1}})
			if err == nil {
				t.Fatalf("short attribute %d accepted", kind)
			}
		})
	}
}

func TestInnerFamilySelectorsRemainWireCoherent(t *testing.T) {
	for _, family := range []innerFamily{innerDual, innerIPv6, innerIPv4} {
		selectors, err := parseTrafficSelectors(innerTrafficSelectors(payloadTSi, family))
		if err != nil {
			t.Fatal(err)
		}
		for _, selector := range selectors {
			if family == innerIPv6 && selector.StartIP.To4() != nil || family == innerIPv4 && selector.StartIP.To4() == nil {
				t.Fatalf("%s emitted wrong selector family", family)
			}
		}
		if family != innerDual && len(selectors) != 1 {
			t.Fatal("single family emitted multiple selectors")
		}
	}
	if usableInnerIP(net.IPv4zero) || usableInnerIP(net.ParseIP("fe80::1")) {
		t.Fatal("invalid inner addresses usable")
	}
}

func TestInnerFamilyIPv4RecoveryAfterAuthenticatedIPv6Mismatch(t *testing.T) {
	installer := &familyInstaller{}
	provider, _ := NewProvider(Config{Timeout: time.Second, Installer: installer})
	calls := 0
	provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
		calls++
		tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, result: familyTestResult()}
		if calls == 1 {
			tr.firstReply = func([]payload) []payload { return []payload{makeNotify(38, nil)} }
		}
		if calls == 2 {
			tr.finalMutation = func(p []payload) []payload {
				return familyReplace(p, payloadCP, func(payload) payload { return familyReplyConfiguration(true, false) })
			}
		}
		return tr, nil
	}
	aka := &testAKAProvider{result: familyTestResult()}
	session, err := provider.Start(context.Background(), familyRequest(aka))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	if calls != 3 || aka.calls != 2 || installer.calls != 1 || installer.config.InnerLocalIPv4 == nil || installer.config.InnerLocalIPv6 != nil {
		t.Fatalf("incomplete IPv4 recovery: calls=%d AKA=%d installs=%d config=%+v", calls, aka.calls, installer.calls, installer.config)
	}
}

func TestInnerFamilyCoherentDualReplyNeedsNoSecondRegistration(t *testing.T) {
	for _, mode := range []string{"ipv4", "ipv6", "dual", "empty-first"} {
		t.Run(mode, func(t *testing.T) {
			installer := &familyInstaller{}
			provider, _ := NewProvider(Config{Timeout: time.Second, Installer: installer})
			calls := 0
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				calls++
				tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, result: familyTestResult()}
				attempt := calls
				tr.finalMutation = func(p []payload) []payload {
					return familyReplace(p, payloadCP, func(v payload) payload {
						switch mode {
						case "ipv6":
							return familyReplyConfiguration(true, true)
						case "dual":
							extra := familyReplyConfiguration(true, true)
							v.Body = append(v.Body, extra.Body[4:]...)
							return v
						case "empty-first":
							if attempt == 1 {
								return payload{Type: payloadCP, Body: []byte{2, 0, 0, 0}}
							}
						}
						return v
					})
				}
				return tr, nil
			}
			aka := &testAKAProvider{result: familyTestResult()}
			session, err := provider.Start(context.Background(), familyRequest(aka))
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close(context.Background())
			want := 1
			if mode == "empty-first" {
				want = 2
			}
			if calls != want || aka.calls != want || installer.calls != 1 {
				t.Fatalf("unnecessary or missing family retry: mode=%s calls=%d AKA=%d installs=%d", mode, calls, aka.calls, installer.calls)
			}
			if mode == "dual" && (installer.config.InnerLocalIPv4 == nil || installer.config.InnerLocalIPv6 == nil) {
				t.Fatal("dual assigned addresses were discarded")
			}
		})
	}
}

func TestInnerFamilyConcurrentModemPlansHaveIndependentBudgets(t *testing.T) {
	var wg sync.WaitGroup
	for _, recover := range []bool{false, true} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			installer := &familyInstaller{}
			provider, _ := NewProvider(Config{Timeout: time.Second, Installer: installer})
			calls := 0
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				calls++
				tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, result: familyTestResult()}
				if recover && calls == 1 {
					tr.firstReply = func([]payload) []payload { return []payload{makeNotify(38, nil)} }
				}
				return tr, nil
			}
			session, err := provider.Start(context.Background(), familyRequest(&testAKAProvider{result: familyTestResult()}))
			if err != nil {
				t.Error(err)
				return
			}
			defer session.Close(context.Background())
			want := 1
			if recover {
				want = 2
			}
			if calls != want || installer.calls != 1 {
				t.Errorf("cross-modem family state: recover=%v calls=%d installs=%d", recover, calls, installer.calls)
			}
		}()
	}
	wg.Wait()
}

func TestInnerFamilyIPv4MappedIPv6IsNotAValidConfigurationOrSelector(t *testing.T) {
	for _, mode := range []string{"cp", "ts"} {
		t.Run(mode, func(t *testing.T) {
			installer := &familyInstaller{}
			provider, _ := NewProvider(Config{Timeout: time.Second, Installer: installer})
			calls := 0
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				calls++
				tr := &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, result: familyTestResult()}
				tr.finalMutation = func(p []payload) []payload {
					if mode == "cp" {
						return familyReplace(p, payloadCP, func(payload) payload {
							v := familyReplyConfiguration(true, true)
							copy(v.Body[8:24], net.ParseIP("::ffff:10.8.0.2").To16())
							return v
						})
					}
					return familyReplace(p, payloadTSi, func(payload) payload {
						v := anyTrafficSelector(true)
						v.Type = payloadTSi
						copy(v.Body[12:28], net.ParseIP("::ffff:10.8.0.2").To16())
						copy(v.Body[28:44], net.ParseIP("::ffff:10.8.0.2").To16())
						return v
					})
				}
				return tr, nil
			}
			session, err := provider.Start(context.Background(), familyRequest(&testAKAProvider{result: familyTestResult()}))
			if session != nil {
				defer session.Close(context.Background())
			}
			if err == nil || calls != 1 || installer.calls != 0 || vowifi.EPDGFallbackAllowed(err) {
				t.Fatalf("mapped IPv6 became IPv4: mode=%s calls=%d installs=%d err=%v", mode, calls, installer.calls, err)
			}
		})
	}
}
