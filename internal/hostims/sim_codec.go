package hostims

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"vocat/internal/vowifi"
)

var (
	ErrSIM               = staticError("host IMS: selected UICC is unavailable or changed")
	ErrSIMApplication    = staticError("host IMS: selected UICC application is unavailable")
	ErrSIMIdentity       = staticError("host IMS: card-provisioned IMS identity is invalid")
	ErrSIMAuthentication = staticError("host IMS: UICC authentication failed")
	ErrSIMReplay         = staticError("host IMS: AKA challenge was already issued")
	ErrQMIProxy          = staticError("host IMS: trusted local qmi-proxy is unavailable")
	ErrQMIEndpoint       = staticError("host IMS: selected modem has no verified QMI endpoint")
)

type uimApplication struct {
	Kind, State byte
	AID         []byte
}
type uimCard struct {
	Slot, Count byte
	USIM, ISIM  []byte
	HasISIM     bool
}

func (uimCard) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private UICC applications>")
}

var usimPrefix = []byte{0xa0, 0, 0, 0, 0x87, 0x10, 2}
var isimPrefix = []byte{0xa0, 0, 0, 0, 0x87, 0x10, 4}

// Parse every card/application, then select the indexed GW-primary UICC. Never
// substitute the first app/card when the primary subscription is invalid.
func parseUIMCardStatus(value []byte) (uimCard, error) {
	var result uimCard
	if len(value) < 9 || len(value) > 8192 {
		return result, ErrSIM
	}
	primary := binary.LittleEndian.Uint16(value[:2])
	cardIndex, appIndex := int(primary>>8), int(primary&255)
	count := int(value[8])
	if count < 1 || count > 5 || primary == 0xffff || cardIndex >= count {
		return result, ErrSIM
	}
	result.Slot, result.Count = byte(cardIndex+1), byte(count)
	offset := 9
	var applications []uimApplication
	selectedCardReady := false
	selectedAppValid := false
	for card := 0; card < count; card++ {
		if len(value)-offset < 6 {
			return result, ErrSIM
		}
		state, appCount := value[offset], int(value[offset+5])
		offset += 6
		if appCount > 16 {
			return result, ErrSIM
		}
		if card == cardIndex {
			selectedCardReady = state == 1
		}
		for app := 0; app < appCount; app++ {
			if len(value)-offset < 7 {
				return result, ErrSIM
			}
			kind, appState, aidLength := value[offset], value[offset+1], int(value[offset+6])
			offset += 7
			if aidLength > 32 || len(value)-offset < aidLength+7 {
				return result, ErrSIM
			}
			aid := value[offset : offset+aidLength]
			offset += aidLength + 7
			if card == cardIndex {
				applications = append(applications, uimApplication{Kind: kind, State: appState, AID: aid})
				if app == appIndex {
					selectedAppValid = kind == 2 && appState == 7 && bytes.HasPrefix(aid, usimPrefix)
				}
			}
		}
	}
	if offset != len(value) || !selectedCardReady || !selectedAppValid {
		return result, ErrSIM
	}
	for index, app := range applications {
		if index == appIndex {
			result.USIM = append([]byte(nil), app.AID...)
		}
		if app.Kind == 5 {
			// An existing but not-ready/broken ISIM is not "absent" and must not
			// authorize a fallback to a possibly different USIM application/key.
			if result.HasISIM || app.State != 7 || !bytes.HasPrefix(app.AID, isimPrefix) {
				return result, ErrSIMApplication
			}
			result.HasISIM = true
			result.ISIM = append([]byte(nil), app.AID...)
		}
	}
	return result, nil
}

func decodeCardICCID(value []byte) (string, error) {
	if len(value) != 10 {
		return "", ErrSIMIdentity
	}
	var digits strings.Builder
	padding := false
	for _, b := range value {
		for _, digit := range []byte{b & 15, b >> 4} {
			if digit == 15 {
				padding = true
				continue
			}
			if digit > 9 || padding {
				return "", ErrSIMIdentity
			}
			digits.WriteByte('0' + digit)
		}
	}
	result := digits.String()
	if !asciiDigits(result, 18, 20) || !strings.HasPrefix(result, "89") {
		return "", ErrSIMIdentity
	}
	return result, nil
}

func decodeCardIMSI(value []byte) (string, error) {
	if len(value) < 2 || int(value[0]) < 1 || int(value[0]) > 8 || int(value[0])+1 > len(value) {
		return "", ErrSIMIdentity
	}
	length := int(value[0])
	for _, b := range value[length+1:] {
		if b != 0xff {
			return "", ErrSIMIdentity
		}
	}
	marker := value[1] & 15
	if marker != 1 && marker != 9 {
		return "", ErrSIMIdentity
	}
	var digits strings.Builder
	first := value[1] >> 4
	if first > 9 {
		return "", ErrSIMIdentity
	}
	digits.WriteByte('0' + first)
	padding := false
	for _, b := range value[2 : length+1] {
		for _, digit := range []byte{b & 15, b >> 4} {
			if digit == 15 {
				padding = true
				continue
			}
			if digit > 9 || padding {
				return "", ErrSIMIdentity
			}
			digits.WriteByte('0' + digit)
		}
	}
	result := digits.String()
	if !asciiDigits(result, 14, 15) || (len(result)%2 == 1) != (marker == 9) {
		return "", ErrSIMIdentity
	}
	return result, nil
}

// A single TS 31.103 identity TLV, followed only by FF file/record padding.
// Do not trim strings: whitespace is not part of a usable IMS identity.
func decodeISIMText(value []byte) (string, error) {
	if len(value) < 2 || len(value) > 1024 || value[0] != 0x80 {
		return "", ErrSIMIdentity
	}
	offset, length := 2, int(value[1])
	switch value[1] {
	case 0x81:
		if len(value) < 3 {
			return "", ErrSIMIdentity
		}
		offset, length = 3, int(value[2])
	case 0x82:
		if len(value) < 4 {
			return "", ErrSIMIdentity
		}
		offset, length = 4, int(binary.BigEndian.Uint16(value[2:4]))
	default:
		if value[1] > 0x7f {
			return "", ErrSIMIdentity
		}
	}
	if length < 1 || length > 512 || length > len(value)-offset {
		return "", ErrSIMIdentity
	}
	for _, b := range value[offset+length:] {
		if b != 0xff {
			return "", ErrSIMIdentity
		}
	}
	text := value[offset : offset+length]
	if !utf8.Valid(text) {
		return "", ErrSIMIdentity
	}
	for _, r := range string(text) {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", ErrSIMIdentity
		}
	}
	return string(text), nil
}

func validPrivateIMSIdentity(value string) bool {
	user, domain, ok := strings.Cut(value, "@")
	if !ok || len(user) < 1 || len(user) > 128 || !validDomain(domain) {
		return false
	}
	for _, c := range []byte(user) {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+.-_%~", rune(c)) {
			return false
		}
	}
	return true
}
func validPublicIMSIdentity(value string) bool {
	scheme, rest, ok := strings.Cut(value, ":")
	if !ok || (scheme != "sip" && scheme != "sips") {
		return false
	}
	if strings.HasSuffix(rest, ";user=phone") {
		rest = strings.TrimSuffix(rest, ";user=phone")
	}
	return validPrivateIMSIdentity(rest)
}

// Standard LV vectors plus the bounded firmware envelopes shared with VoSharp
// and Rust. Validate the complete borrowed layout before copying any key.
func parseCardAKA(raw []byte) (vowifi.AKAResult, error) {
	if len(raw) < 3 || len(raw) > 1024 || raw[len(raw)-2] != 0x90 || raw[len(raw)-1] != 0 {
		return vowifi.AKAResult{}, ErrSIMAuthentication
	}
	data := raw[:len(raw)-2]
	if data[0] == 0x80 {
		if len(data) < 2 {
			return vowifi.AKAResult{}, ErrSIMAuthentication
		}
		offset, length := 2, int(data[1])
		if data[1] == 0x81 {
			if len(data) < 3 {
				return vowifi.AKAResult{}, ErrSIMAuthentication
			}
			offset, length = 3, int(data[2])
		} else if data[1] > 0x7f {
			return vowifi.AKAResult{}, ErrSIMAuthentication
		}
		if length == 0 || length != len(data)-offset {
			return vowifi.AKAResult{}, ErrSIMAuthentication
		}
		data = data[offset:]
	}
	if len(data) < 2 {
		return vowifi.AKAResult{}, ErrSIMAuthentication
	}
	for _, start := range []int{1, 2} {
		if start == 2 && int(data[1]) != len(data)-2 {
			continue
		}
		offset := start
		take := func() ([]byte, bool) {
			if offset >= len(data) {
				return nil, false
			}
			n := int(data[offset])
			offset++
			if n > len(data)-offset {
				return nil, false
			}
			v := data[offset : offset+n]
			offset += n
			return v, true
		}
		if data[0] == 0xdc {
			auts, ok := take()
			if ok && len(auts) == 14 && offset == len(data) {
				return vowifi.AKAResult{AUTS: append([]byte(nil), auts...), SynchronizationFailure: true}, nil
			}
			continue
		}
		if data[0] != 0xdb {
			break
		}
		res, ok := take()
		if !ok || len(res) < 4 || len(res) > 16 {
			continue
		}
		ck, ok := take()
		if !ok || len(ck) != 16 {
			continue
		}
		ik, ok := take()
		if !ok || len(ik) != 16 {
			continue
		}
		if offset < len(data) {
			kc, ok := take()
			if !ok || len(kc) != 8 {
				continue
			}
		}
		if offset != len(data) {
			continue
		}
		return vowifi.AKAResult{RES: append([]byte(nil), res...), CK: append([]byte(nil), ck...), IK: append([]byte(nil), ik...)}, nil
	}
	return vowifi.AKAResult{}, ErrSIMAuthentication
}

func clearCardAKA(result *vowifi.AKAResult) {
	clear(result.RES)
	clear(result.CK)
	clear(result.IK)
	clear(result.AUTS)
	*result = vowifi.AKAResult{}
}
