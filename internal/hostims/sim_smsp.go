package hostims

import "context"

var ErrSIMSMSCenter = staticError("host IMS: the selected SIM has no usable SMS service-centre address")

// EF_SMSP record 1, TS 31.102 section 4.2.27. The optional alpha identifier
// precedes a fixed 28-octet parameter block; PI bit 2 is zero when SCA exists.
// Never replace missing or malformed provisioning with a guessed carrier number.
func decodeCardSMSCenter(record []byte) string {
	if len(record) < 28 || len(record) > 255 {
		return ""
	}
	parameters := record[len(record)-28:]
	if parameters[0]&2 != 0 {
		return ""
	}
	sca := parameters[13:25]
	length := int(sca[0])
	if length < 2 || length > 11 || sca[1]&0x80 == 0 || sca[1]&15 != 1 {
		return ""
	}
	ton := sca[1] & 0x70
	if ton != 0 && ton != 0x10 && ton != 0x20 {
		return ""
	}
	digits := make([]byte, 0, 20)
	for i, value := range sca[2 : length+1] {
		low, high := value&15, value>>4
		if low > 9 || high > 9 && (high != 15 || i != length-2) {
			return ""
		}
		digits = append(digits, '0'+low)
		if high != 15 {
			digits = append(digits, '0'+high)
		}
	}
	if len(digits) < 3 {
		return ""
	}
	if ton == 0x10 {
		return "+" + string(digits)
	}
	return string(digits)
}

func (t *cardTransaction) readSMSCenter(ctx context.Context) (string, error) {
	raw, err := readCardFile(ctx, t.session, t.card.Slot, t.channel, 0x6f42, true)
	defer clear(raw)
	if t.session.Uncertain() {
		return "", ErrUncertain
	}
	if err == ErrSIMIdentity {
		// Optional file absent/inaccessible: voice registration remains usable.
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return decodeCardSMSCenter(raw), nil
}
