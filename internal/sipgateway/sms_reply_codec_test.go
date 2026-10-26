package sipgateway

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

const smsReplyCodecICCID = "8900000000000000001"

func legacySMSReplyAliasForCodecTest(device, subscription, number string) string {
	return "sms." + base64.RawURLEncoding.EncodeToString([]byte(device)) + "." + hashCallOperation(subscription) + "." + strings.TrimPrefix(number, "+")
}

func TestSMSReplyCodecProvenFullNumberRoundTripsWithOriginalCard(t *testing.T) {
	for _, number := range []string{"+15551234567", "15551234567", "1234567", "+123456789012345"} {
		t.Run(number, func(t *testing.T) {
			user := compactSMSReplyUser("line-a", smsReplyCodecICCID, number)
			if !strings.HasPrefix(user, "sms2-") || len(user) > 78 {
				t.Fatalf("reply alias is not compact: %q", user)
			}
			deviceHash, binding, parsed, ok := parseCompactSMSReplyUser(user)
			if !ok || deviceHash != hashCallOperation("line-a")[:12] || binding != hashCallOperation(smsReplyCodecICCID) || parsed != number {
				t.Fatal("compact reply lost its device, full original card digest, or number")
			}
			bytes, err := hex.DecodeString(binding)
			if err != nil || len(bytes) != 32 {
				t.Fatal("card binding was truncated")
			}
			if user == compactSMSReplyUser("line-b", smsReplyCodecICCID, number) || user == compactSMSReplyUser("line-a", "8900000000000000002", number) {
				t.Fatal("another original device/card inherited the reply identity")
			}
		})
	}
}

func TestSMSReplyCodecMaximumAliasIsBelowAsteriskExtensionLimit(t *testing.T) {
	device := strings.Repeat("a", 64)
	user := compactSMSReplyUser(device, strings.Repeat("9", 22), "+123456789012345")
	if len(user) != 78 {
		t.Fatalf("maximum compact alias length=%d, want 78", len(user))
	}
	if _, _, _, ok := parseCompactSMSReplyUser(user); !ok {
		t.Fatal("longest legal source did not round-trip")
	}
	// The old representation exceeds the 79-character extension buffer even
	// though the exact same physical device/card/number are otherwise valid.
	if len(legacySMSReplyAliasForCodecTest(device, strings.Repeat("9", 22), "+123456789012345")) <= 79 {
		t.Fatal("regression fixture no longer demonstrates the legacy alias risk")
	}
}

func TestSMSReplyCodecUnprovenOrNonTelephoneSourcesStayReadOnly(t *testing.T) {
	for _, fixture := range [][3]string{
		{"line-a", "", "+15551234567"},
		{"line-a", "synthetic-unproven-subscription", "+15551234567"},
		{"line-a", strings.Repeat("1", 17), "+15551234567"},
		{"line-a", strings.Repeat("1", 23), "+15551234567"},
		{"line-a", "８９００００００００００００００００１", "+15551234567"},
		{"line-a", smsReplyCodecICCID, "BANK"},
		{"line-a", smsReplyCodecICCID, "1069"},
		{"line-a", smsReplyCodecICCID, "123456"},
		{"line-a", smsReplyCodecICCID, "+1234567890123456"},
		{"line-a", smsReplyCodecICCID, "01234567"},
		{"line-a", smsReplyCodecICCID, "+1 5551234567"},
		{"line-a", smsReplyCodecICCID, "+15551234567\n"},
		{"line-a", smsReplyCodecICCID, "+15551234567\r\nX: injected"},
		{"", smsReplyCodecICCID, "+15551234567"},
		{"line-a\n", smsReplyCodecICCID, "+15551234567"},
	} {
		user := compactSMSReplyUser(fixture[0], fixture[1], fixture[2])
		if !strings.HasPrefix(user, "sms-ro-") || len(user) != 71 || user != compactSMSReplyUser(fixture[0], fixture[1], fixture[2]) {
			t.Fatal("unproven source did not produce a stable, compact read-only identity")
		}
		if _, _, _, ok := parseCompactSMSReplyUser(user); ok {
			t.Fatal("read-only alias parsed as a payable reply")
		}
	}
}

func TestSMSReplyCodecRejectsMalformedTruncatedOrLegacyAliases(t *testing.T) {
	valid := compactSMSReplyUser("line-a", smsReplyCodecICCID, "+15551234567")
	for _, user := range []string{
		"sms2-", valid + "junk", valid + ";line=other", valid + "\n", valid + "\r\n", "sip:" + valid,
		valid[:len(valid)-7], valid[:17] + "-unknown-+15551234567",
		"sms2-012345abcdef-" + hashCallOperation(smsReplyCodecICCID) + "-+15551234567",
		legacySMSReplyAliasForCodecTest("line-a", smsReplyCodecICCID, "+15551234567"),
		"sms-ro-" + strings.Repeat("a", 64),
	} {
		if device, binding, number, ok := parseCompactSMSReplyUser(user); ok || device != "" || binding != "" || number != "" {
			t.Fatalf("malformed reply accepted or leaked partial result: %q", user)
		}
	}
}

func TestSMSReplyCodecRejectsAlternateBase64PaddingBits(t *testing.T) {
	valid := compactSMSReplyUser("line-a", smsReplyCodecICCID, "+15551234567")
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, valid[60])
	if last < 0 || last&3 != 0 {
		t.Fatal("invalid canonical fixture")
	}
	alternate := valid[:60] + string(alphabet[last+1]) + valid[61:]
	if _, _, _, ok := parseCompactSMSReplyUser(alternate); ok {
		t.Fatal("noncanonical unused padding bits created another spelling of a reply")
	}
}

func TestSMSReplyCodecPayloadAndGoDeviceIdentityRemainCaseSensitive(t *testing.T) {
	valid := compactSMSReplyUser("line-a", smsReplyCodecICCID, "+15551234567")
	if valid == compactSMSReplyUser("LINE-A", smsReplyCodecICCID, "+15551234567") {
		t.Fatal("case-sensitive Go device IDs became the same original route")
	}
	upperPrefixAndHex := "SMS2-" + strings.ToUpper(valid[5:17]) + valid[17:]
	if device, binding, _, ok := parseCompactSMSReplyUser(upperPrefixAndHex); !ok || device != compactSMSReplyDeviceHash("line-a") || binding != hashCallOperation(smsReplyCodecICCID) {
		t.Fatal("fixed prefix/hex case normalization changed the card payload")
	}
	for index := 18; index < 60; index++ { // Leave the final padding bits intact.
		character := valid[index]
		var changed byte
		if character >= 'A' && character <= 'Z' {
			changed = character + ('a' - 'A')
		} else if character >= 'a' && character <= 'z' {
			changed = character - ('a' - 'A')
		} else {
			continue
		}
		alternate := valid[:index] + string(changed) + valid[index+1:]
		_, binding, _, ok := parseCompactSMSReplyUser(alternate)
		if !ok || binding == hashCallOperation(smsReplyCodecICCID) {
			t.Fatal("case-folded base64 payload was accepted as the original card")
		}
		return
	}
	t.Fatal("fixture has no case-sensitive payload character")
}

func TestSMSReplyCodecReservesRetiredAndReadOnlyNamespaces(t *testing.T) {
	for _, user := range []string{"sms2-", "SMS2-invalid", "sms-ro-hash", "SMS-old", "sms.old.parts", "SMS.bad"} {
		if !isSMSReplyNamespace(user) {
			t.Fatalf("reply namespace could fall through to a default paid route: %q", user)
		}
	}
	for _, user := range []string{"1001", "+15551234567", "sms2", "smservice", "sip:sms2-invalid"} {
		if isSMSReplyNamespace(user) {
			t.Fatalf("ordinary target was incorrectly reserved: %q", user)
		}
	}
}
