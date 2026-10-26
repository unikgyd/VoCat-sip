package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"vocat/internal/sipgateway"
)

func TestCallAnswerClaimSingleWinnerAndUncertainSubmissionNotReplayed(t *testing.T) {
	s := &Server{}
	var winners atomic.Int32
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			release, err := s.claimCallAnswer(context.Background(), "a", "original")
			if err == nil {
				winners.Add(1)
				release(false)
			} else if !errors.Is(err, sipgateway.ErrCarrierBusy) {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if winners.Load() != 1 {
		t.Fatal("multiple automatic/browser/SIP answers admitted", winners.Load())
	}
	if _, err := s.claimCallAnswer(context.Background(), "a", "original"); !errors.Is(err, sipgateway.ErrCarrierBusy) {
		t.Fatal("uncertain answer replayed", err)
	}
	for _, target := range [][2]string{{"b", "original"}, {"a", "next-call"}} {
		release, err := s.claimCallAnswer(context.Background(), target[0], target[1])
		if err != nil {
			t.Fatal("unrelated call blocked", err)
		}
		release(true)
	}
}

func TestCallAnswerNotAttemptedReleaseCannotEraseNewOwner(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	old, err := s.claimCallAnswer(ctx, "a", "call")
	if err != nil {
		t.Fatal(err)
	}
	old(true)
	current, err := s.claimCallAnswer(ctx, "a", "call")
	if err != nil {
		t.Fatal(err)
	}
	old(true)
	if _, err := s.claimCallAnswer(ctx, "a", "call"); !errors.Is(err, sipgateway.ErrCarrierBusy) {
		t.Fatal("old release erased current answer claim", err)
	}
	current(false)
}
