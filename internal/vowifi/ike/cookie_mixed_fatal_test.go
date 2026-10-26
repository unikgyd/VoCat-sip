package ike

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"vocat/internal/vowifi"
)

func TestCookieMixedFatalCannotAuthorizeFallback(t *testing.T) {
	for _, kind := range []uint16{14, 17} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			provider, _ := NewProvider(Config{Installer: unusedInstaller{}, AutoProposalFallback: true})
			var opened []*cookieParityTransport
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				data := []byte(nil)
				if kind == 17 {
					data = []byte{0, 2}
				}
				tr := &cookieParityTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t, cookieChallenge: []byte{1}}, invalid: []payload{makeNotify(notifyCookie, []byte{1}), makeNotify(kind, data)}}
				opened = append(opened, tr)
				return tr, nil
			}
			aka := &testAKAProvider{}
			request := familyRequest(aka)
			request.Identity = vowifi.SIMIdentity{ICCID: "8900000000000000001", IMSI: "999990123456789", HomeMCC: "999", HomeMNC: "99"}
			if vowifi.ResolveCarrierProfile(request.Identity).ID != vowifi.CarrierProfileStandard {
				t.Fatal("fixture must exercise automatic proposal fallback")
			}
			_, err := provider.Start(context.Background(), request)
			var access *vowifi.EPDGAccessError
			if err == nil || len(opened) != 1 || errors.As(err, &access) || aka.calls != 0 {
				t.Fatalf("ambiguous COOKIE enabled another connection/access proof: opened=%d AKA=%d error=%v", len(opened), aka.calls, err)
			}
			for _, tr := range opened {
				if tr.calls != 1 || tr.closes != 1 {
					t.Fatalf("calls=%d closes=%d", tr.calls, tr.closes)
				}
			}
		})
	}
}

func TestCookieMixedControlPureProposalStillHasAccessProof(t *testing.T) {
	provider, _ := NewProvider(Config{Installer: unusedInstaller{}})
	tr := &cookieParityTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t, cookieChallenge: []byte{1}}, invalid: []payload{makeNotify(notifyNoProposal, nil)}}
	provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
		return tr, nil
	}
	aka := &testAKAProvider{}
	_, err := provider.Start(context.Background(), familyRequest(aka))
	var access *vowifi.EPDGAccessError
	if !errors.As(err, &access) || tr.calls != 1 || tr.closes != 1 || aka.calls != 0 {
		t.Fatalf("calls=%d closes=%d AKA=%d err=%v", tr.calls, tr.closes, aka.calls, err)
	}
}
