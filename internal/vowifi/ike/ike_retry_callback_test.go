package ike

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"vocat/internal/vowifi"
)

func TestIKERetryAdmissionRunsBeforeAnyInitDatagram(t *testing.T) {
	provider, err := NewProvider(Config{Installer: unusedInstaller{}})
	if err != nil {
		t.Fatal(err)
	}
	tr := &initialAuthTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, mode: "rsa"}
	provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
		return tr, nil
	}
	want := errors.New("synthetic admission denied")
	request := familyRequest(&testAKAProvider{})
	admissions := 0
	request.BeforeIKE = func(context.Context) error { admissions++; return want }
	_, err = provider.Start(context.Background(), request)
	if !errors.Is(err, want) || admissions != 1 || tr.calls != 0 || tr.closes != 1 || tr.deleted {
		t.Fatalf("send before admission: admissions=%d calls=%d closes=%d delete=%v err=%v", admissions, tr.calls, tr.closes, tr.deleted, err)
	}
}

func TestIKERetryNetworkShapedAdmissionFailureDoesNotAuthorizeFamilyOrLegacyFallback(t *testing.T) {
	provider, err := NewProvider(Config{Installer: unusedInstaller{}, AutoProposalFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := &initialAuthTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, mode: "rsa"}
	provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
		return tr, nil
	}
	request := familyRequest(&testAKAProvider{})
	request.BeforeIKE = func(context.Context) error { return errors.Join(vowifi.ErrIKERetryStore, syscall.EHOSTUNREACH) }
	_, err = provider.Start(context.Background(), request)
	var access *vowifi.EPDGAccessError
	if !errors.Is(err, vowifi.ErrIKERetryStore) || errors.As(err, &access) || vowifi.EPDGFallbackAllowed(err) || tr.calls != 0 || tr.closes != 1 {
		t.Fatalf("policy became a transport recovery: calls=%d closes=%d err=%v", tr.calls, tr.closes, err)
	}
}

func TestIKERetryAuthenticatedCallbackPrecedesCleanupAndRetainsCommitFailure(t *testing.T) {
	key, der, roots := backoffCertificates(t)
	for _, mode := range []string{"commit-error", "cancel", "untrusted"} {
		t.Run(mode, func(t *testing.T) {
			provider, err := NewProvider(Config{Installer: unusedInstaller{}, RootCAs: roots, ServerName: "epdg.example"})
			if err != nil {
				t.Fatal(err)
			}
			tr := &backoffTranscript{initialAuthTransport: &initialAuthTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, mode: "rsa", key: key, certificate: der}, behavior: "network"}
			if mode == "untrusted" {
				tr.behavior = "no-auth"
			}
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				return tr, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			failure := errors.New("synthetic commit failure")
			request := familyRequest(&testAKAProvider{})
			request.ObserveNetworkRejection = func(proof vowifi.EPDGNetworkRejection) error {
				calls++
				if tr.deleted || tr.closes != 0 || proof.NotifyType != 10500 || proof.EPDG != "epdg.example" || proof.APN != "ims" {
					t.Errorf("callback not before cleanup: %+v", proof)
				}
				if mode == "cancel" {
					cancel()
					return nil
				}
				return failure
			}
			_, err = provider.Start(ctx, request)
			if err == nil || !tr.deleted || tr.closes != 1 {
				t.Fatalf("cleanup missing: delete=%v closes=%d err=%v", tr.deleted, tr.closes, err)
			}
			if mode == "untrusted" {
				if calls != 0 {
					t.Fatal("unauthenticated error called trusted observer")
				}
				return
			}
			if calls != 1 {
				t.Fatalf("observer calls=%d", calls)
			}
			if mode == "commit-error" && !errors.Is(err, failure) {
				t.Fatal("commit error erased")
			}
			var proof *vowifi.EPDGNetworkRejection
			if !errors.As(err, &proof) {
				t.Fatal("authenticated evidence erased", err)
			}
		})
	}
}
