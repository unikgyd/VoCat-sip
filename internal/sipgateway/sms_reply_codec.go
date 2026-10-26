package sipgateway

import (
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
)

// The full card digest is encoded as 43 base64url characters, not 64 hex
// characters. With a 15-digit international number and its optional '+', the
// complete user part is at most 78 bytes (below Asterisk's 79-character buffer).
// A digest is only a routing identity; it never grants paid sending permission.
var compactSMSReplyPattern = regexp.MustCompile(`\A[sS][mM][sS]2-([0-9a-fA-F]{12})-([A-Za-z0-9_-]{43})-(\+?[1-9][0-9]{6,14})\z`)
var compactSMSReplyNumberPattern = regexp.MustCompile(`\A\+?[1-9][0-9]{6,14}\z`)

func validSMSReplyICCID(subscription string) bool {
	if len(subscription) < 18 || len(subscription) > 22 {
		return false
	}
	for _, character := range subscription {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// Go device IDs and their line ACLs are case sensitive. Unlike the Windows
// case-insensitive line keys, do not lowercase an ID into another configured
// device. The route resolver must reject zero OR multiple matching devices.
func compactSMSReplyDeviceHash(device string) string {
	if !deviceIDPattern.MatchString(device) {
		return ""
	}
	return hashCallOperation(device)[:12]
}

func compactSMSReplyUser(device, subscription, number string) string {
	deviceHash := compactSMSReplyDeviceHash(device)
	if deviceHash == "" || !validSMSReplyICCID(subscription) || !compactSMSReplyNumberPattern.MatchString(number) {
		return "sms-ro-" + hashCallOperation("sip-sms-read-only-v1\x00"+device+"\x00"+subscription+"\x00"+number)
	}
	binding, _ := hex.DecodeString(hashCallOperation(subscription)) // SHA-256 is always 32 bytes.
	return "sms2-" + deviceHash + "-" + base64.RawURLEncoding.EncodeToString(binding) + "-" + number
}

// The transport user and base64url payload are case sensitive. Only the fixed
// ASCII prefix and hex device digest admit alternate case; the digest is
// normalized before route lookup. Noncanonical padding bits are rejected.
func parseCompactSMSReplyUser(user string) (deviceHash, subscriptionBinding, number string, ok bool) {
	match := compactSMSReplyPattern.FindStringSubmatch(user)
	if match == nil {
		return "", "", "", false
	}
	binding, err := base64.RawURLEncoding.Strict().DecodeString(match[2])
	if err != nil || len(binding) != 32 || base64.RawURLEncoding.EncodeToString(binding) != match[2] {
		return "", "", "", false
	}
	return strings.ToLower(match[1]), hex.EncodeToString(binding), match[3], true
}

// Reserve legacy and read-only prefixes as well: malformed/retired aliases must
// not silently enter the ordinary/default phone-number route.
func isSMSReplyNamespace(user string) bool {
	user = strings.ToLower(user)
	return strings.HasPrefix(user, "sms.") || strings.HasPrefix(user, "sms-") || strings.HasPrefix(user, "sms2-")
}
