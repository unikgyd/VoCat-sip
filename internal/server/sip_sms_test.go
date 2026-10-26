package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"vocat/internal/developer"
	"vocat/internal/device"
	"vocat/internal/modem"
	"vocat/internal/sipgateway"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type boundSMSController struct {
	fakeVoWiFiController
	sent        int
	imsi, iccid string
}

func (c *boundSMSController) SendSMSForSubscription(_ context.Context, _ string, imsi, iccid string, request vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error) {
	c.sent++
	c.imsi = imsi
	c.iccid = iccid
	return vowifi.SMSSubmitResult{To: request.Recipient, PartsTotal: 1, PartsAttempted: 1, PartsAccepted: 1, AllPartsAccepted: true, SubmissionStatus: "accepted"}, nil
}
func TestSIPSMSStrictAdapterQuotaHistoryAndSIMBinding(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.UpsertDevice(context.Background(), store.Device{ID: "module-a", Name: "Synthetic SMS"}); err != nil {
		t.Fatal(err)
	}
	controller := &boundSMSController{fakeVoWiFiController: fakeVoWiFiController{state: vowifi.State{Enabled: true, SIMReady: true, IMSReady: true, SMSReady: true, IMSI: "001010000000001", ICCID: "synthetic-sim"}}}
	s := &Server{store: db, vowifi: controller, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), devices: fakeDeviceController{entry: device.Device{ID: "module-a", Discovered: true, Snapshot: &device.Snapshot{IMSI: "001010000000001", ICCID: "synthetic-sim"}}, atHandler: func(string) (modem.Response, error) {
		t.Fatal("SMS bridge must never use AT")
		return modem.Response{}, nil
	}}}
	b := sipSMSBackend{s}
	token, err := b.SMSReady(context.Background(), "module-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.SendCarrierSMS(context.Background(), "module-a", "+15550000001", "synthetic", "wrong", "wrong-sim"); !errors.Is(err, sipgateway.ErrCarrierUnavailable) || controller.sent != 0 {
		t.Fatal("SIM binding bypassed", err)
	}
	if err = developer.SetSMSHourlyLimit(context.Background(), db, 1); err != nil {
		t.Fatal(err)
	}
	result, err := b.SendCarrierSMS(context.Background(), "module-a", "+15550000001", "synthetic", "safe-operation", token)
	if err != nil || !result.AllPartsAccepted || controller.imsi != "001010000000001" || controller.iccid != "synthetic-sim" {
		t.Fatal(result, err)
	}
	if _, err = b.SendCarrierSMS(context.Background(), "module-a", "+15550000001", "synthetic", "another-operation", token); !errors.Is(err, sipgateway.ErrSMSRateLimited) || controller.sent != 1 {
		t.Fatal("global quota bypassed", err)
	}
	rows, err := db.ListSMSMessages(context.Background(), store.SMSFilter{})
	if err != nil || len(rows) != 1 || rows[0].MessageID != "sip-submit:safe-operation" || rows[0].Body != "synthetic" || rows[0].ICCID != "synthetic-sim" {
		t.Fatal("history missing", rows, err)
	}
	controller.state.SMSReady = false
	if _, err = b.SMSReady(context.Background(), "module-a"); !errors.Is(err, sipgateway.ErrCarrierUnavailable) {
		t.Fatal("SMS readiness ignored")
	}
}
func TestSIPSMSDurableInboxCursorAndIncompleteMessageFilter(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "sip-sms-cursor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Server{store: db}
	s.sipGateway = sipgateway.New(context.Background(), db)
	defer s.sipGateway.Close()
	if smsMessageReadyToNotify(store.SMSMessage{MessageID: "concat:synthetic", Body: "partial", Extra: []byte(`{"concat_complete":false}`)}) {
		t.Fatal("incomplete multipart SMS was forwardable")
	}
	// Disabled by default: consider incoming rows but do not forward history
	// when the feature is subsequently enabled.
	row, err := db.SaveSMSMessage(context.Background(), store.SMSMessage{MessageID: "synthetic-mt", DeviceID: "module-a", Peer: "+15550000001", Body: "inbox", Direction: "inbound"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.syncSIPSMSOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	cursor, err := s.sipGateway.SMSCursor(context.Background(), 999)
	if err != nil || cursor != row.ID {
		t.Fatal(cursor, err)
	}
	s.sipGateway.Close()
	s.sipGateway = sipgateway.New(context.Background(), db)
	if cursor, err = s.sipGateway.SMSCursor(context.Background(), 999); err != nil || cursor != row.ID {
		t.Fatal("cursor not durable", cursor, err)
	}
}
