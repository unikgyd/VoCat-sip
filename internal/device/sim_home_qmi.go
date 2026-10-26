package device

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"vocat/internal/modem"
)

type qmiHomeFileSession interface {
	GetIMSI(context.Context) (string, error)
	GetUSIMAID(context.Context) ([]byte, error)
	OpenLogicalChannel(context.Context, uint8, []byte) (byte, error)
	CloseLogicalChannel(context.Context, uint8, uint8) error
	SendAPDU(context.Context, uint8, uint8, []byte) ([]byte, error)
}

// Read the active USIM's EF_AD directly. The dependency's GetNativeMCCMNC
// prefers OPL and silently defaults to two digits, neither is home evidence.
func readQMIHomePLMN(ctx context.Context, s qmiHomeFileSession) (mcc, mnc string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	imsi, err := s.GetIMSI(ctx)
	if err != nil {
		return "", "", err
	}
	if !homeSubscriberDigits(imsi) {
		return "", "", errors.New("QMI returned an invalid home IMSI")
	}
	aid, err := s.GetUSIMAID(ctx)
	if err != nil {
		return "", "", err
	}
	if len(aid) < 7 || len(aid) > 32 || !bytes.HasPrefix(aid, []byte{0xA0, 0, 0, 0, 0x87, 0x10, 2}) {
		return "", "", errors.New("QMI home-file read requires an explicit USIM AID")
	}
	channel, err := s.OpenLogicalChannel(ctx, 1, aid)
	if err != nil {
		return "", "", err
	}
	if channel == 0 {
		return "", "", errors.New("QMI returned the unowned basic channel")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if closeErr := s.CloseLogicalChannel(cleanup, 1, channel); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close QMI home-file channel: %w", closeErr))
		}
	}()
	if channel > 19 {
		return "", "", errors.New("QMI returned an invalid home-file channel")
	}
	// The dependency's provisioning IMSI and its first USIM AID need not refer
	// to the same app/slot. Prove the selected logical channel is this subscriber.
	_, sw, err := qmiHomeAPDU(ctx, s, channel, []byte{0, 0xA4, 0, 0x0C, 2, 0x6F, 7})
	if err != nil {
		return "", "", err
	}
	if sw != 0x9000 && sw>>8 != 0x91 {
		return "", "", fmt.Errorf("select QMI home IMSI status %04X", sw)
	}
	rawIMSI, sw, err := qmiHomeAPDU(ctx, s, channel, []byte{0, 0xB0, 0, 0, 9})
	if err != nil {
		return "", "", err
	}
	if (sw != 0x9000 && sw>>8 != 0x91) || !homeIMSIFileMatches(rawIMSI, imsi) {
		return "", "", errors.New("QMI selected USIM does not match the live IMSI")
	}
	mcc = imsi[:3]
	// SELECT EF_AD, no FCP requested; READ BINARY only. No VERIFY, UPDATE,
	// AUTHENTICATE, profile switch or proactive FETCH is issued here.
	_, sw, err = qmiHomeAPDU(ctx, s, channel, []byte{0, 0xA4, 0, 0x0C, 2, 0x6F, 0xAD})
	if err != nil {
		return "", "", err
	}
	if sw == 0x6A82 {
		return mcc, "", modem.ErrHomeMNCLengthUnavailable
	}
	if sw != 0x9000 && sw>>8 != 0x91 {
		return "", "", fmt.Errorf("select QMI EF_AD status %04X", sw)
	}
	data, sw, err := qmiHomeAPDU(ctx, s, channel, []byte{0, 0xB0, 0, 0, 4})
	if err != nil {
		return "", "", err
	}
	if sw == 0x6282 && len(data) < 4 {
		return mcc, "", modem.ErrHomeMNCLengthUnavailable
	}
	if sw != 0x9000 && sw>>8 != 0x91 {
		return "", "", fmt.Errorf("read QMI EF_AD status %04X", sw)
	}
	if len(data) < 4 {
		return mcc, "", modem.ErrHomeMNCLengthUnavailable
	}
	length := int(data[3] & 0x0F)
	if length != 2 && length != 3 {
		return mcc, "", modem.ErrHomeMNCLengthUnavailable
	}
	return mcc, imsi[3 : 3+length], nil
}

func homeIMSIFileMatches(raw []byte, imsi string) bool {
	if !homeSubscriberDigits(imsi) {
		return false
	}
	marker := byte(1)
	if len(imsi)%2 != 0 {
		marker = 9
	}
	expected := []byte{byte(1 + len(imsi)/2), (imsi[0]-'0')<<4 | marker}
	for i := 1; i < len(imsi); i += 2 {
		high := byte(15)
		if i+1 < len(imsi) {
			high = imsi[i+1] - '0'
		}
		expected = append(expected, (imsi[i]-'0')|high<<4)
	}
	if len(raw) < len(expected) || !bytes.Equal(raw[:len(expected)], expected) {
		return false
	}
	for _, b := range raw[len(expected):] {
		if b != 0xFF {
			return false
		}
	}
	return true
}

func homeSubscriberDigits(value string) bool {
	if len(value) < 6 || len(value) > 16 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func qmiHomeAPDU(ctx context.Context, s qmiHomeFileSession, channel byte, command []byte) ([]byte, uint16, error) {
	var accumulated []byte
	for step := 0; step < 4; step++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		raw, err := s.SendAPDU(ctx, 1, channel, command)
		if err != nil {
			return nil, 0, err
		}
		if len(raw) < 2 {
			return nil, 0, errors.New("QMI home-file APDU omitted status")
		}
		last := len(raw) - 2
		sw := uint16(raw[last])<<8 | uint16(raw[last+1])
		if sw>>8 == 0x6C && len(command) == 5 {
			command = append([]byte(nil), command...)
			command[4] = byte(sw)
			continue
		}
		if sw>>8 == 0x61 {
			accumulated = append(accumulated, raw[:last]...)
			if len(accumulated) > 256 {
				return nil, 0, errors.New("QMI home-file response exceeded APDU bound")
			}
			command = []byte{0, 0xC0, 0, 0, byte(sw)}
			continue
		}
		if len(accumulated)+last > 256 {
			return nil, 0, errors.New("QMI home-file response exceeded APDU bound")
		}
		return append(accumulated, raw[:last]...), sw, nil
	}
	return nil, 0, errors.New("QMI home-file continuation limit reached")
}
