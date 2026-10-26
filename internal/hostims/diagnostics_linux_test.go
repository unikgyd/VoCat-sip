//go:build linux

package hostims

import (
	"errors"
	"fmt"
	"testing"
)

func TestPreparationDiagnosticsNeverExposeUntrustedErrorText(t *testing.T) {
	if got := PreparationFailureCode(fmt.Errorf("private APDU/subscriber payload: %w", ErrSIMIdentity)); got != "sim_identity" {
		t.Fatal("trusted failure code was not preserved", got)
	}
	if got := PreparationFailureCode(errors.New("private APDU/subscriber payload")); got != "unclassified" {
		t.Fatal("unknown error text escaped", got)
	}
}
