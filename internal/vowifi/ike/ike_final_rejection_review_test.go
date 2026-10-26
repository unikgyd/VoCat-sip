package ike

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

// The existing transcript does real DH, encrypted IKE_AUTH, AKA MAC and MSK
// AUTH. Only SIM responses, datagram IO and the CHILD_SA installer are synthetic.
func TestFinalRejectionReviewDeferredAUTHMustAuthenticateBeforePolicyCapture(t *testing.T) {
	for _, mode := range []string{"network", "apn", "apn-id", "deactivated", "missing-timer", "zero", "cleanup-fails", "invalid-auth", "missing-auth", "duplicate-auth", "wrong-transcript", "mixed-error", "timer-only"} {
		t.Run(mode, func(t *testing.T) {
			installer := &familyInstaller{}
			provider, err := NewProvider(Config{Timeout: time.Second, Installer: installer, AutoProposalFallback: true})
			if err != nil {
				t.Fatal(err)
			}
			var tr *familyTranscript
			reachedFinal, attempts, observations := false, 0, 0
			wantProof := mode == "network" || mode == "apn" || mode == "apn-id" || mode == "deactivated" || mode == "missing-timer" || mode == "zero" || mode == "cleanup-fails"
			code := uint16(10500)
			if mode == "apn" {
				code = 9002
			}
			timer := []byte{1, 1} // 600 seconds, not an invented wait.
			if mode == "deactivated" {
				timer = []byte{1, 0xe0}
			}
			if mode == "zero" {
				timer = []byte{1, 0}
			}
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				attempts++
				tr = &familyTranscript{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, result: familyTestResult()}
				if mode == "cleanup-fails" {
					tr.deleteErr = errors.New("synthetic cleanup failure")
				}
				tr.finalMutation = func(reply []payload) []payload {
					reachedFinal = true
					auth, parseErr := onePayload(reply, payloadAuth)
					if parseErr != nil {
						t.Fatal(parseErr)
					}
					if len(tr.msk) != 64 {
						t.Fatal("fixture never completed EAP key derivation")
					}
					if mode == "invalid-auth" {
						auth.Body[len(auth.Body)-1] ^= 1
					}
					output := []payload{auth}
					if mode == "missing-auth" {
						output = nil
					}
					if mode == "duplicate-auth" {
						output = append(output, auth)
					}
					if mode == "wrong-transcript" {
						other := payload{Type: payloadIDr, Body: []byte{2, 0, 0, 0, 'x'}}
						signed, signErr := responderSignedOctets(tr.response, tr.nonceI, tr.suite, tr.keys.SKpr, other)
						if signErr != nil {
							t.Fatal(signErr)
						}
						pad, _ := prf(tr.suite, tr.msk, []byte("Key Pad for IKEv2"))
						mic, _ := prf(tr.suite, pad, signed)
						output[0] = payload{Type: payloadAuth, Body: append([]byte{authMethodSharedKeyMIC, 0, 0, 0}, mic...)}
					}
					if mode == "apn-id" {
						// Preserve the existing final APN-ID compatibility: the MIC
						// remains bound to the original responder transcript.
						output = append(output, payload{Type: payloadIDr, Body: []byte{2, 0, 0, 0, 'i', 'm', 's'}})
					}
					if mode != "timer-only" {
						output = append(output, makeNotify(code, nil))
					}
					if mode == "mixed-error" {
						output = append(output, makeNotify(24, nil))
					}
					if mode != "missing-timer" {
						output = append(output, makeNotify(41041, timer))
					}
					return output // A rejected CHILD_SA need not include SA/TS/CP.
				}
				return tr, nil
			}
			aka := &testAKAProvider{result: familyTestResult()}
			request := familyRequest(aka)
			request.ObserveNetworkRejection = func(proof vowifi.EPDGNetworkRejection) error {
				observations++
				if tr.deletes != 0 || tr.closes != 0 {
					t.Fatal("policy observation happened after cleanup")
				}
				if proof.NotifyType != code || proof.EPDG != request.EPDG || proof.APN != "ims" || net.ParseIP(proof.Endpoint) == nil {
					t.Fatalf("unbound proof: %#v", proof)
				}
				return nil
			}
			_, startErr := provider.Start(context.Background(), request)
			if startErr == nil || !reachedFinal || aka.calls != 1 || attempts != 1 || installer.calls != 0 {
				t.Fatalf("not a final-AUTH refusal: final=%v AKA=%d attempts=%d installs=%d error=%v", reachedFinal, aka.calls, attempts, installer.calls, startErr)
			}
			if tr.deletes != 1 || tr.closes != 1 || vowifi.EPDGFallbackAllowed(startErr) {
				t.Fatalf("unsafe cleanup/retry: deletes=%d closes=%d error=%v", tr.deletes, tr.closes, startErr)
			}
			var proof *vowifi.EPDGNetworkRejection
			if errors.As(startErr, &proof) != wantProof || observations != map[bool]int{true: 1, false: 0}[wantProof] {
				t.Fatalf("final MSK AUTH policy capture = %v, callbacks=%d, want proof=%v: %v", proof != nil, observations, wantProof, startErr)
			}
			if wantProof {
				status := vowifi.IKEBackoffTimed
				if mode == "missing-timer" {
					status = vowifi.IKEBackoffMissing
				}
				if mode == "deactivated" {
					status = vowifi.IKEBackoffDeactivated
				}
				if proof.NotifyType != code || proof.Backoff.Status != status {
					t.Fatalf("wrong authenticated proof: %#v", proof)
				}
				if status == vowifi.IKEBackoffTimed {
					wantDuration := 10 * time.Minute
					if mode == "zero" {
						wantDuration = 0
					}
					if proof.Backoff.Duration != wantDuration {
						t.Fatal("timer changed during proof capture")
					}
				}
			}
		})
	}
}
