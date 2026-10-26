package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"vocat/internal/smsquery"
)

func TestSMSAPIReadOnlyStoredIdentityAndLiteralFilters(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Truncate(time.Second)
	var original []SMSMessage
	for i, v := range []SMSMessage{
		{DeviceID: "original-a", Peer: "+15550000001", Direction: "inbound", Body: "登录 %_' 验证码：１２３４５６", Timestamp: now.Add(-time.Minute)},
		{DeviceID: "original-b", Peer: "+15550000001", Direction: "received", Body: "登录 验证码 654321", Timestamp: now.Add(-30 * time.Second)},
		{DeviceID: "original-a", Peer: "+15550000001", Direction: "outbound", Body: "登录 验证码 777777", Timestamp: now},
		{DeviceID: "original-a", Peer: "OTHER", Direction: "inbound", Body: "登录 验证码 888888", Timestamp: now.Add(time.Hour)},
		{DeviceID: "original-a", Peer: "OTHER", Direction: "inbound", Body: "登录 验证码 999999", Timestamp: now.Add(-8 * 24 * time.Hour)},
	} {
		v.MessageID = string(rune('a' + i))
		v.IMSI = "private-imsi"
		v.ICCID = "private-iccid"
		v.Extra = []byte(`{"pdu":"private-pdu"}`)
		saved, err := db.SaveSMSMessage(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		original = append(original, saved)
	}
	q, err := smsquery.Parse("keyword=%25_%27&sender=%2B15550000001&slotId=original-a", false, now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.QueryReadOnlySMS(ctx, q)
	if err != nil || len(result.Messages) != 1 || result.Messages[0].Code == nil || *result.Messages[0].Code != "123456" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	for _, v := range original {
		actual, err := db.SMSMessage(ctx, v.ID)
		if err != nil || !reflect.DeepEqual(v, actual) {
			t.Fatalf("query modified saved message %d", v.ID)
		}
	}
	q, _ = smsquery.Parse("", false, now)
	result, err = db.QueryReadOnlySMS(ctx, q)
	if err != nil || len(result.Messages) != 2 || *result.Messages[0].SlotID != "original-b" {
		t.Fatalf("wrong ordering/direction/time %+v %v", result, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = db.QueryReadOnlySMS(cancelled, q); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestSMSAPICandidateBoundAndUnicodeTruncation(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "bound.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Truncate(time.Second)
	// Batch transaction keeps this a realistic SQLite regression without 1000
	// independent durable forwarding admissions for synthetic archive entries.
	values := make([]SMSMessage, 1001)
	for i := range values {
		values[i] = SMSMessage{DeviceID: "line", Peer: "SHOP", Direction: "inbound", Body: "订单号123456", Timestamp: now}
	}
	if err = db.SaveSMSMessages(ctx, values); err != nil {
		t.Fatal(err)
	}
	q, _ := smsquery.Parse("", true, now)
	result, err := db.QueryReadOnlySMS(ctx, q)
	if err != nil || len(result.Messages) != 0 || !result.ScanLimited {
		t.Fatalf("unbounded OTP scan %+v %v", result, err)
	}
	_, err = db.SaveSMSMessage(ctx, SMSMessage{DeviceID: "line", Peer: "SHOP", Direction: "inbound", Body: strings.Repeat("a", 4095) + "😀 验证码 123456", Timestamp: now})
	if err != nil {
		t.Fatal(err)
	}
	result, err = db.QueryReadOnlySMS(ctx, q)
	if err != nil || len(result.Messages) != 1 || result.ScanLimited || !result.Messages[0].TextTruncated || smsquery.Units(result.Messages[0].Text) != 4095 {
		t.Fatalf("truncation %+v %v", result, err)
	}
}
