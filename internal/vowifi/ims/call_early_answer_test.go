package ims

import (
	"context"
	"strings"
	"testing"
)

func TestOutgoingFinalAnswerKeepsOnlyMatchingEarlyDialogMedia(t *testing.T) {
	for _, tc := range []struct {
		name, earlyTag, finalTag, finalBody string
		early                               bool
		wantActive                          bool
	}{
		{name: "same dialog without repeated SDP", early: true, earlyTag: "selected", finalTag: "selected", wantActive: true},
		{name: "different fork needs its own answer", early: true, earlyTag: "early", finalTag: "other"},
		{name: "untagged early answer is insufficient", early: true, finalTag: "selected"},
		{name: "no early answer is insufficient", finalTag: "selected"},
		{name: "invalid final SDP must not fall back", early: true, earlyTag: "selected", finalTag: "selected", finalBody: "invalid SDP"},
		{name: "new final SDP is applied", early: true, earlyTag: "selected", finalTag: "selected", finalBody: strings.Replace(callTestSDP, "19000", "19002", 1), wantActive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, conn := newCallTestSession(t)
			acceptCallTestControls(s, conn)
			public, err := s.DialCall(context.Background(), "+15550000002")
			if err != nil {
				t.Fatal(err)
			}
			s.callMu.Lock()
			call := s.calls[public.ID]
			s.callMu.Unlock()
			to := func(tag string) string {
				if tag == "" {
					return "<tel:+15550000002>"
				}
				return "<tel:+15550000002>;tag=" + tag
			}
			if tc.early {
				call.responses <- &sipResponse{StatusCode: 183, Headers: map[string][]string{"to": {to(tc.earlyTag)}}, Body: []byte(callTestSDP)}
				waitCallTest(t, func() bool { return s.Calls()[0].State == "early_media" })
			}
			call.responses <- &sipResponse{StatusCode: 180, Reason: "Ringing", Headers: map[string][]string{"to": {to(tc.earlyTag)}}}
			call.responses <- &sipResponse{StatusCode: 200, Reason: "OK", Headers: map[string][]string{"to": {to(tc.finalTag)}}, Body: []byte(tc.finalBody)}
			waitCallTest(t, func() bool {
				state := s.Calls()[0].State
				return state == "active" || state == "failed"
			})
			got := s.Calls()[0]
			if (got.State == "active") != tc.wantActive || conn.count("ACK") != 1 {
				t.Fatalf("final answer state=%s media=%t ACK=%d", got.State, got.MediaReady, conn.count("ACK"))
			}
			if tc.wantActive {
				if got.SIPCode != 200 || got.Reason != "OK" {
					t.Fatalf("final response retained provisional diagnostics: %#v", got)
				}
				if !got.MediaReady || got.Codec != "PCMA" || got.AnsweredAt == nil {
					t.Fatalf("valid answer lost negotiated media: %#v", got)
				}
				if tc.finalBody != "" {
					call.media.mu.RLock()
					port := call.media.remote.Port
					call.media.mu.RUnlock()
					if port != 19002 {
						t.Fatal("final SDP was ignored")
					}
				}
				if err := s.HangupCall(context.Background(), public.ID); err != nil {
					t.Fatal(err)
				}
			}
			waitCallTest(t, func() bool { return conn.count("BYE") == 1 })
		})
	}
}
