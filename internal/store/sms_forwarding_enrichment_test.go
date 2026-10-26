package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestSMSForwardExistingInboxIdentityEnrichmentCannotAcquireAnotherPlan(t *testing.T) {
	for _, mutation := range []string{"unknown-hardware-filled", "known-hardware-changed", "message-id-edited"} {
		t.Run(mutation, func(t *testing.T) {
			database := smsForwardTestStore(t, filepath.Join(t.TempDir(), "identity-enrichment.db"))
			ctx := context.Background()
			calls := 0
			database.SetSMSForwardPlanner(func(input SMSForwardingInput) (json.RawMessage, error) {
				calls++
				return json.Marshal(map[string]string{"first_plan": input.SourceKey})
			})
			message := smsForwardTestMessage("identity-is-not-a-new-network-delivery")
			if mutation == "unknown-hardware-filled" {
				message.ModemIMEI = ""
			}
			first, err := database.SaveSMSMessage(ctx, message)
			if err != nil || calls != 1 {
				t.Fatal("initial live source did not atomically capture its plan:", err)
			}
			before, err := database.AppSetting(ctx, smsForwardLedgerKey)
			if err != nil {
				t.Fatal(err)
			}
			message = first
			switch mutation {
			case "message-id-edited":
				message.MessageID = "edited-source-id"
			case "known-hardware-changed":
				message.ModemIMEI = "867394042309831"
			default:
				message.ModemIMEI = "867394042309830"
			}
			changed, err := database.SaveSMSMessage(ctx, message)
			if err != nil || changed.ID != first.ID || SMSForwardSourceKey(changed) == SMSForwardSourceKey(first) || calls != 1 {
				t.Fatal("an identity edit of the same Inbox acquired a new recipient plan:", err)
			}
			if matches, err := database.SMSForwardSourceMatches(ctx, SMSForwardInboxGuard{ID: first.ID, Fingerprint: SMSForwardFingerprint(first)}); err != nil || matches {
				t.Fatal("old payload guard survived a source-identity edit:", err)
			}
			after, err := database.AppSetting(ctx, smsForwardLedgerKey)
			if err != nil || string(before.Value) != string(after.Value) || before.Sensitive != after.Sensitive {
				t.Fatal("identity enrichment rewrote the original recipient plan:", err)
			}
			if _, err := database.SaveSMSMessage(ctx, changed); err != nil || calls != 1 {
				t.Fatal("repeating the enriched source bypassed its empty receipt:", err)
			}
			fresh := smsForwardTestMessage("fresh-positive-after-source-enrichment")
			if _, err := database.SaveSMSMessage(ctx, fresh); err != nil || calls != 2 {
				t.Fatal("source isolation disabled unrelated actual Inbox planning:", err)
			}
		})
	}
}
