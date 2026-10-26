package sipgateway

import (
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
)

var prefixedInternationalSMS = regexp.MustCompile(`^[0-9]{1,8}\+[1-9][0-9]{6,14}$`)

// Matches the shared C#/Rust contract, not a network address or Via branch.
func smsOperationID(realm, account, callID string, sequence uint32) string {
	return hashCallOperation(strings.Join([]string{"sip-sms-v1", strings.ToLower(realm), strings.ToLower(account), callID, strconv.FormatUint(uint64(sequence), 10)}, "\x00"))
}
func smsReplyUser(device, subscription, number string) string {
	return compactSMSReplyUser(device, subscription, number)
}

// Return the exact original card binding for either a new compact reply or a
// fully valid legacy reply. Never recover a truncated user part from To, the
// default line, or the currently inserted card.
func smsReplyBinding(target string) string {
	if _, binding, _, ok := parseCompactSMSReplyUser(target); ok {
		return binding
	}
	_, binding, _, ok := parseLegacySMSReply(target)
	if ok {
		return binding
	}
	return ""
}

func parseLegacySMSReply(target string) (device, binding, number string, ok bool) {
	// Older aliases can exceed Asterisk's 79-character extension boundary.
	// At exactly that boundary, a retained full hash plus a shortened-but-valid
	// phone number is indistinguishable from truncation. Refuse it rather than
	// sending to that shorter number. New compact aliases never reach it.
	if len(target) == 79 {
		return "", "", "", false
	}
	parts := strings.Split(target, ".")
	if len(parts) != 4 || parts[0] != "sms" || !smsHash(parts[2]) {
		return "", "", "", false
	}
	id, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(id) != parts[1] || !deviceIDPattern.MatchString(string(id)) ||
		!internationalNumberPattern.MatchString("+"+parts[3]) {
		return "", "", "", false
	}
	return string(id), strings.ToLower(parts[2]), "+" + parts[3], true
}

func smsRoute(config Config, account Account, target string) (CarrierLine, string, int) {
	if !config.SMSEnabled {
		return CarrierLine{}, "", 405
	}
	if isSMSReplyNamespace(target) {
		deviceHash, _, number, compact := parseCompactSMSReplyUser(target)
		legacyDevice := ""
		if !compact {
			var valid bool
			legacyDevice, _, number, valid = parseLegacySMSReply(target)
			if !valid {
				if strings.HasPrefix(strings.ToLower(target), "sms-ro-") {
					return CarrierLine{}, "", 403
				}
				return CarrierLine{}, "", 400
			}
		}
		var selected CarrierLine
		matches := 0
		for _, line := range config.CarrierLines {
			if compact && compactSMSReplyDeviceHash(line.DeviceID) == deviceHash || !compact && line.DeviceID == legacyDevice {
				selected = line
				matches++
			}
		}
		if matches == 0 {
			return CarrierLine{}, "", 404
		}
		if matches != 1 {
			return CarrierLine{}, "", 400
		}
		if !strings.HasPrefix(number, "+") {
			number = "+" + number
		}
		return smsRouteOnLine(selected, account, number)
	}
	// Reuse the exact prefix/default/normalization policy, but never voice
	// permissions. A known extension is resolved before this method is called.
	copy := cloneConfig(config)
	copy.CarrierEnabled = true
	for i := range copy.CarrierLines {
		copy.CarrierLines[i].AllowOutgoing = copy.CarrierLines[i].AllowSMSSend
	}
	account.AllowCalls = true
	account.OutgoingLines = account.SMSSendLines
	return carrierRoute(copy, account, target)
}
func smsRouteOnLine(line CarrierLine, account Account, number string) (CarrierLine, string, int) {
	permitted := false
	for _, id := range account.SMSSendLines {
		if id == line.DeviceID {
			permitted = true
		}
	}
	if !line.AllowSMSSend || !permitted {
		return CarrierLine{}, "", 403
	}
	if !internationalNumberPattern.MatchString(number) {
		return CarrierLine{}, "", 484
	}
	return line, number, 0
}
