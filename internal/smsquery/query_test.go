package smsquery

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSMSAPISharedQueryContract(t *testing.T) {
	raw, err := os.ReadFile("../../../contracts/sms-api-query-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Version int
		Now     string
		Cases   []struct {
			Name, Raw, Keyword, Sender, SlotID string
			OTP, Valid                         bool
			Limit, AgeSeconds                  int
			OTPOnly                            *bool
		}
		KeywordCases []struct {
			Text, Keyword string
			Matches       bool
		}
	}
	if err = json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Version != 1 || len(contract.Cases) != 32 || len(contract.KeywordCases) != 11 {
		t.Fatal("contract drift")
	}
	now, err := time.Parse(time.RFC3339, contract.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range contract.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			q, err := Parse(tc.Raw, tc.OTP, now)
			if (err == nil) != tc.Valid {
				t.Fatalf("valid=%v: %v", tc.Valid, err)
			}
			if !tc.Valid {
				return
			}
			if tc.Limit != 0 && tc.Limit != q.Limit || tc.AgeSeconds != 0 && int(now.Sub(q.Since).Seconds()) != tc.AgeSeconds || tc.OTPOnly != nil && *tc.OTPOnly != q.OTPOnly || tc.Keyword != "" && tc.Keyword != q.Keyword || tc.Sender != "" && tc.Sender != q.Sender || tc.SlotID != "" && tc.SlotID != q.SlotID {
				t.Fatalf("query = %+v", q)
			}
		})
	}
	for _, tc := range contract.KeywordCases {
		t.Run("keyword-"+tc.Keyword+"-"+tc.Text, func(t *testing.T) {
			if Contains(tc.Text, tc.Keyword) != tc.Matches {
				t.Fatal("Unicode matching drift")
			}
		})
	}
}
func TestSMSAPISharedOTPContract(t *testing.T) {
	raw, err := os.ReadFile("../../../contracts/otp-extraction-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Version int
		Cases   []struct {
			Text string
			Code *string
		}
	}
	if err = json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Version != 1 || len(contract.Cases) != 21 {
		t.Fatal("contract drift")
	}
	for _, tc := range contract.Cases {
		t.Run(tc.Text, func(t *testing.T) {
			actual := ExtractCode(tc.Text)
			if (actual == nil) != (tc.Code == nil) || actual != nil && *actual != *tc.Code {
				t.Fatalf("extract %q = %v expected %v", tc.Text, actual, tc.Code)
			}
		})
	}
}
func TestSMSAPIUnicodeBoundaries(t *testing.T) {
	now := time.Now()
	for _, raw := range []string{"keyword=%FF", "keyword=" + strings.Repeat("😀", 101), "sender=" + strings.Repeat("a", 101), "slotId=" + strings.Repeat("a", 129), "keyword=%C2%85"} {
		if _, err := Parse(raw, false, now); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, tc := range []struct {
		Text      string
		Units     int
		Truncated bool
	}{{strings.Repeat("a", 4095) + "😀", 4095, true}, {strings.Repeat("a", 4094) + "😀", 4096, false}} {
		text, truncated := Truncate(tc.Text)
		if Units(text) != tc.Units || truncated != tc.Truncated {
			t.Fatal("split emoji or incorrect boundary")
		}
	}
}
