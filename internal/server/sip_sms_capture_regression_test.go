package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"vocat/internal/auth"
	"vocat/internal/sipgateway"
	"vocat/internal/store"
)

// This file intentionally uses only APIs present in the accepted pre-capture
// source archive. It can be copied by itself onto that archive for a genuine
// RED run: no newly introduced hook/plan/receipt/source-guard symbol is used.
// Server.New constructs the actual sipgateway.Manager, with actual file SQLite
// and a loopback-only SIP listener. No modem or external service is supplied.
func newSIPSMSCaptureRegression(t *testing.T, enabled bool) (*Server, *store.Store) {
	t.Helper()
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "capture-regression.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	authentication, err := auth.New(database, auth.Options{SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Store: database, Auth: authentication,
		Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>synthetic capture fixture</html>")}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseSIPGateway)
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.LocalAddr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	config := sipgateway.DefaultConfig()
	config.Enabled, config.SMSEnabled = true, enabled
	config.Listen = address
	config.AllowedCIDRs = []string{"127.0.0.0/8"}
	config.Accounts = []sipgateway.Account{
		{Username: "1001", Password: "synthetic-capture-password-one"},
		{Username: "1002", Password: "synthetic-capture-password-two"},
	}
	config.CarrierLines = []sipgateway.CarrierLine{{DeviceID: "module-a", Label: "Synthetic capture line",
		AllowSMSReceive: true, SMSRecipients: []string{"1001"}}}
	view, err := s.sipGateway.Apply(ctx, config)
	if err != nil || !view.Status.Running {
		t.Fatal("actual SIP configuration could not start:", err)
	}
	return s, database
}

type sipSMSCaptureRecordedInbox struct {
	Message struct {
		ID string `json:"id"`
	} `json:"message"`
	Targets map[string]json.RawMessage `json:"targets"`
}

func sipSMSCaptureTargets(t *testing.T, database *store.Store, messageID string) map[string]json.RawMessage {
	t.Helper()
	setting, err := database.AppSetting(context.Background(), "sip.gateway.sms-ledger.private.v1")
	if errors.Is(err, store.ErrNotFound) {
		return nil // A durable empty plan need not contain a private payload.
	}
	if err != nil {
		t.Fatal(err)
	}
	var ledger struct {
		Inbox map[string]sipSMSCaptureRecordedInbox `json:"inbox"`
	}
	if !setting.Sensitive || json.Unmarshal(setting.Value, &ledger) != nil || ledger.Inbox == nil {
		t.Fatal("private SMS journal is not a valid sensitive inbox document")
	}
	var found map[string]json.RawMessage
	for _, inbox := range ledger.Inbox {
		if inbox.Message.ID == messageID {
			if found != nil {
				t.Fatal("one original source acquired multiple forwarding payloads")
			}
			found = inbox.Targets
		}
	}
	return found
}

func sipSMSCaptureMessage(id string) store.SMSMessage {
	return store.SMSMessage{MessageID: id, DeviceID: "module-a", ModemIMEI: "860000000000001",
		ICCID: "8900000000000000001", IMSI: "001010000000001", Peer: "+15550000001",
		Direction: "inbound", Body: "synthetic original private message", Source: "ims", PartsTotal: 1,
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func TestSIPSMSCaptureRegressionRecipientPlanBelongsToStoreReceiveNotLaterPoll(t *testing.T) {
	s, database := newSIPSMSCaptureRegression(t, true)
	message := sipSMSCaptureMessage("capture-before-recipient-change")
	if _, err := database.SaveSMSMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	// No poll happened while 1001 held the original receive grant. Saving a
	// new configuration must not grant 1002 this already-received private SMS.
	changed := s.sipGateway.Snapshot().Config
	changed.CarrierLines[0].SMSRecipients = []string{"1002"}
	if _, err := s.sipGateway.Apply(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if err := s.syncSIPSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	targets := sipSMSCaptureTargets(t, database, message.MessageID)
	if len(targets) != 1 || targets["1001"] == nil || targets["1002"] != nil {
		t.Fatalf("poll-time configuration reassigned the original private SMS: original=1001, targets=%v", targets)
	}
}

func TestSIPSMSCaptureRegressionDisabledReceiveDoesNotBackfillOnFirstEnable(t *testing.T) {
	s, database := newSIPSMSCaptureRegression(t, false)
	message := sipSMSCaptureMessage("capture-while-sms-disabled")
	if _, err := database.SaveSMSMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	// The first SMS cursor/poll is deliberately AFTER enabling a newly created
	// recipient. Advancing a cursor while disabled is not a sufficient fix.
	enabled := s.sipGateway.Snapshot().Config
	enabled.SMSEnabled = true
	enabled.Accounts = []sipgateway.Account{{Username: "1003", Password: "synthetic-new-capture-password"}}
	enabled.CarrierLines[0].SMSRecipients = []string{"1003"}
	if _, err := s.sipGateway.Apply(context.Background(), enabled); err != nil {
		t.Fatal(err)
	}
	if err := s.syncSIPSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if targets := sipSMSCaptureTargets(t, database, message.MessageID); len(targets) != 0 {
		t.Fatalf("enabling SMS retroactively exposed a disabled-era source to a new account: %v", targets)
	}
	rows, err := database.ListSMSMessages(context.Background(), store.SMSFilter{})
	if err != nil || len(rows) != 1 || rows[0].Body != message.Body {
		t.Fatal("disabled forwarding must still preserve the user's original normal inbox:", err)
	}
}

func TestSIPSMSCaptureRegressionDeletedInboxRedeliveryCannotAcquireNewRecipientPlan(t *testing.T) {
	s, database := newSIPSMSCaptureRegression(t, true)
	message := sipSMSCaptureMessage("capture-deleted-inbox-source-replay")
	saved, err := database.SaveSMSMessage(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteSMSMessage(context.Background(), saved.ID); err != nil {
		t.Fatal(err)
	}
	changed := s.sipGateway.Snapshot().Config
	changed.CarrierLines[0].SMSRecipients = []string{"1002"}
	if _, err := s.sipGateway.Apply(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	// A modem rescan/redelivery has the same original source identity. Normal
	// inbox deletion must not delete the independent original forwarding receipt.
	if _, err := database.SaveSMSMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := s.syncSIPSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if targets := sipSMSCaptureTargets(t, database, message.MessageID); targets["1002"] != nil || len(targets) > 1 {
		t.Fatalf("deleting the inbox row allowed source replay to acquire a new private recipient: %v", targets)
	}
}
