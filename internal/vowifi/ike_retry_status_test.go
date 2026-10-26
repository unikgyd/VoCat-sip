package vowifi

import (
	"errors"
	"fmt"
	"testing"
)

func TestIKERetryStatusBindingFailureIsTypedWithoutTextMatching(t *testing.T) {
	for _, err := range []error{ErrIKERetryBinding, fmt.Errorf("outer: %w", ErrIKERetryBinding), errors.Join(errors.New("transport closed"), ErrIKERetryBinding)} {
		status := ikeRetryStatusForError(err)
		if status == nil || status.Source != "binding-unavailable" || !status.UntilCardEvent {
			t.Fatalf("binding failure lost structured status: %+v", status)
		}
	}
	if status := ikeRetryStatusForError(errors.New("vowifi: IKE retry attempt lost its SIM lifecycle binding")); status != nil {
		t.Fatalf("text invented typed status: %+v", status)
	}
}
