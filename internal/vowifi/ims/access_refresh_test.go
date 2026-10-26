package ims

import (
	"context"
	"errors"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

type budgetCellularBearer struct {
	*fakeCellularBearer
	budget time.Duration
}

func (b *budgetCellularBearer) VerificationTimeout() time.Duration { return b.budget }

func TestCellularVerificationBudgetIsBoundedAndPreservesCaller(t *testing.T) {
	for _, value := range []time.Duration{-1, 0, time.Second, 3 * time.Second, 20 * time.Second, 25 * time.Second, time.Minute} {
		b := &budgetCellularBearer{budget: value}
		want := value
		if want < 3*time.Second || want > 25*time.Second {
			want = 3 * time.Second
		}
		if got := cellularVerificationBudget(b); got != want {
			t.Fatalf("budget %v: got %v want %v", value, got, want)
		}
	}
	request := testCellularRequest()
	request.Cellular = &budgetCellularBearer{fakeCellularBearer: request.Cellular.(*fakeCellularBearer), budget: 20 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := verifiedCellularEvidence(ctx, request); !errors.Is(err, ErrCellularAccess) {
		t.Fatal("longer source budget ignored caller cancellation", err)
	}
}

func TestCellularFreshCellUpdatesSessionPANIWithoutChangingAccess(t *testing.T) {
	request := testCellularRequest()
	bearer := request.Cellular.(*fakeCellularBearer)
	old, err := verifiedCellularEvidence(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{provider: &Provider{config: Config{Port: 5060}}, request: request, access: old, initialEndpoint: pcscfEndpoint{host: "198.51.100.1", port: 5060}}
	s.setPANI(old.PANI)
	bearer.mu.Lock()
	bearer.evidence.Cell = vowifi.LTECell{MCC: "111", MNC: "22", Duplex: "TDD", TAC: 0x44d5, CellID: 0x6543210}
	newPANI, _ := bearer.evidence.Cell.PAccessNetworkInfo()
	bearer.mu.Unlock()
	if err := s.verifyCellularAccess(context.Background()); err != nil || s.pAccessNetworkInfo() != newPANI || old.PANI == newPANI {
		t.Fatal("same PDN cell transition kept registration-time PANI", err)
	}
	bearer.mu.Lock()
	bearer.evidence.LocalIPv4 = "192.0.2.99"
	bearer.evidence.Cell.CellID++
	bearer.mu.Unlock()
	if err := s.verifyCellularAccess(context.Background()); !errors.Is(err, ErrCellularAccess) || s.pAccessNetworkInfo() != newPANI {
		t.Fatal("failed bearer verification replaced the last proven PANI", err)
	}
}
