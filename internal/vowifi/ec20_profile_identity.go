package vowifi

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"vocat/internal/modem"
)

// EF_IMSI is not an ICCID BCD string: it has a byte count, identity/parity
// nibble and (only for even digit counts) a final F nibble. Never truncate a
// claimed length or skip an interior F. TS 31.102 4.2.2 / TS 24.008 10.5.1.4.
func parseProfileIMSI(raw []byte) (string, error) {
	bad := errors.New("vowifi: invalid EF_IMSI encoding")
	if len(raw) < 2 {
		return "", bad
	}
	length := int(raw[0])
	if length < 1 || length > 8 || length+1 > len(raw) {
		return "", bad
	}
	marker := raw[1] & 15
	if marker != 1 && marker != 9 {
		return "", bad
	}
	digits := []byte{raw[1] >> 4}
	if digits[0] > 9 {
		return "", bad
	}
	for i := 2; i <= length; i++ {
		low, high := raw[i]&15, raw[i]>>4
		if low > 9 {
			return "", bad
		}
		digits = append(digits, low)
		if marker == 1 && i == length {
			if high != 15 {
				return "", bad
			}
		} else {
			if high > 9 {
				return "", bad
			}
			digits = append(digits, high)
		}
	}
	if len(digits) < 6 || len(digits) > 15 || len(digits)%2 == 0 && marker != 1 || len(digits)%2 != 0 && marker != 9 {
		return "", bad
	}
	for _, b := range raw[length+1:] {
		if b != 255 {
			return "", bad
		}
	}
	for i := range digits {
		digits[i] += '0'
	}
	return string(digits), nil
}

func (adapter *EC20Adapter) readProfileIMSI(ctx context.Context, deviceID string) (string, error) {
	commands := []string{"AT+CRSM=176,28423,0,0,9", `AT+CRSM=176,28423,0,0,9,"","3F007FFF"`}
	for _, command := range commands {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		response, err := adapter.executeSensitive(ctx, deviceID, command)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			var commandErr *modem.CommandError
			if errors.As(err, &commandErr) && (strings.EqualFold(strings.TrimSpace(commandErr.Final), "ERROR") || strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(commandErr.Final)), " ", "") == "+CMEERROR:4") {
				continue
			}
			return "", errors.New("vowifi: EF_IMSI transport or SIM access failed")
		}
		fields := parseCSV(valueAfterATPrefix(response, "+CRSM:"))
		if len(fields) >= 2 {
			sw1, e1 := strconv.Atoi(fields[0])
			sw2, e2 := strconv.Atoi(fields[1])
			if e1 == nil && e2 == nil && sw1 >= 0 && sw1 <= 255 && sw2 >= 0 && sw2 <= 255 && ((sw1 == 0x6a && (sw2 == 0x82 || sw2 == 0x83 || sw2 == 0x86)) || (sw1 == 0x94 && sw2 == 4) || sw1 == 0x6d || sw1 == 0x6e) {
				continue
			}
		}
		raw, err := parseCRSMData(response)
		if err != nil {
			return "", errors.New("vowifi: EF_IMSI read was not a completed, valid file response")
		}
		return parseProfileIMSI(raw)
	}
	return "", nil // explicit absence/unsupported only; do not guess a subscriber
}
