package sipgateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"vocat/internal/store"
)

// A present empty JSON record is corrupt history, not an absent record. This
// narrow pure-parser test complements the actual file-Store rollback tests.
func TestSMSCapturePresentEmptyPrivateDocumentsNeverBecomeAbsentHistory(t *testing.T) {
	_, database, _, _ := smsCaptureFixture(t)
	settings, err := database.AppSetting(context.Background(), SettingKey)
	if err != nil {
		t.Fatal(err)
	}
	message := smsCaptureMessage("empty-private-record", "synthetic private body")
	message.ID = 1
	input := store.SMSForwardingInput{Message: message, Settings: settings, Now: time.Now(),
		SourceKey: store.SMSForwardSourceKey(message), Fingerprint: store.SMSForwardFingerprint(message),
		OriginalPeer: message.Peer, OriginalICCID: message.ICCID}
	for _, name := range []string{"ledger", "settings"} {
		t.Run(name, func(t *testing.T) {
			corrupt := input
			empty := json.RawMessage(make([]byte, 0)) // non-nil: present but empty
			if name == "ledger" {
				corrupt.Ledger = store.AppSetting{Key: SMSLedgerSettingKey, Sensitive: true, Value: empty}
			} else {
				corrupt.Settings.Value = empty
			}
			if data, err := captureSMSForwarding(corrupt); err == nil || data != nil {
				t.Fatal("present corrupt private history was replaced by an empty/new journal")
			}
		})
	}
	input.Settings.Value = nil // actual absence: optional service has no grant
	if data, err := captureSMSForwarding(input); err != nil || data != nil {
		t.Fatal("an absent optional configuration must retain an empty source plan:", err)
	}
}
