package hostims

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"vocat/internal/vowifi"
)

const nasGetCellLocationInfo = uint16(0x0043)

// nasLTEServingCell is a decoded, single-response candidate, NOT an ownership,
// freshness or Established proof. A future live source must own the NAS CID,
// pin the selected modem/SIM/PDN, match the solicited transaction, bound the
// query lifetime, and observe registration/cell changes through delivery.
// This decoder never queries a modem or upgrades the MM cached source.
type nasLTEServingCell struct {
	cell   vowifi.LTECell
	earfcn uint32
	pci    uint16
	band   uint16
	idle   bool
}

func (nasLTEServingCell) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private single-response NAS LTE candidate; not a live proof>")
}

// decodeNASLTECellLocationFrame requires an actual NAS response envelope, not
// an indication/request or a packet assembled from independent NAS/MM reads.
// The common proxy decoder enforces exact QMUX/TLV lengths and no duplicates.
// The returned scalar values do not alias frame.
func decodeNASLTECellLocationFrame(frame []byte, cid byte, transaction uint16) (nasLTEServingCell, error) {
	if cid == 0 || cid == 255 || transaction == 0 {
		return nasLTEServingCell{}, ErrServingCellUnavailable
	}
	packet, err := decodeProxyPacket(frame)
	if err != nil || len(frame) < 13 || frame[6] != 2 || packet.ServiceType != qmi.ServiceNAS ||
		packet.ClientID != cid || packet.TransactionID != transaction || packet.MessageID != nasGetCellLocationInfo || packet.IsIndication {
		return nasLTEServingCell{}, ErrServingCellUnavailable
	}
	var lte, result []byte
	for _, tlv := range packet.TLVs {
		switch tlv.Type {
		case 2:
			result = tlv.Value
		case 0x13:
			lte = tlv.Value
		case 0x14, 0x15, 0x16:
			// Known LTE inter-frequency / neighboring GERAN / WCDMA data
			// cannot supply or override ANY serving PLMN/TAC/ECI/duplex field.
		case 0x1e:
			if len(tlv.Value) != 4 {
				return nasLTEServingCell{}, ErrServingCellUnavailable
			}
		default:
			// This profile deliberately does not accept other serving RATs,
			// NR/NSA or unknown vendor/extended-EARFCN overlays. In particular
			// do not combine a 16-bit EARFCN with an unproven correction TLV.
			return nasLTEServingCell{}, ErrServingCellUnavailable
		}
	}
	if len(result) != 4 || binary.LittleEndian.Uint16(result) != 0 || binary.LittleEndian.Uint16(result[2:]) != 0 {
		return nasLTEServingCell{}, ErrServingCellUnavailable
	}
	return decodeNASLTEIntraFrequency(lte)
}

// Wire facts are independently implemented from the public libqmi NAS schema:
// https://github.com/linux-mobile-broadband/libqmi/blob/main/data/qmi-service-nas.json
// NAS 0x0043, LTE IntraFrequency TLV 0x13: idle:u8, PLMN:3 octets,
// TAC:u16le, ECI:u32le, EARFCN:u16le, serving PCI:u16le, four u8 thresholds,
// count:u8, then count * (PCI:u16le + four i16le measurements).
// "v2" changes the API's PLMN representation, not EARFCN into a 32-bit field.
// A nonconforming firmware that wraps an extended EARFCN into 16 bits cannot
// be distinguished by this payload. Real firmware/schema acceptance is still
// required; accepting the scalar layout is not acceptance of a live source.
func decodeNASLTEIntraFrequency(value []byte) (nasLTEServingCell, error) {
	const header, measurement = 19, 10
	if len(value) < header || value[0] > 1 || len(value) != header+int(value[18])*measurement {
		return nasLTEServingCell{}, ErrServingCellUnavailable
	}
	mcc, mnc, ok := decodeNASPLMN(value[1:4])
	if !ok {
		return nasLTEServingCell{}, ErrServingCellUnavailable
	}
	tac := binary.LittleEndian.Uint16(value[4:6])
	eci := binary.LittleEndian.Uint32(value[6:10])
	earfcn := uint32(binary.LittleEndian.Uint16(value[10:12]))
	pci := binary.LittleEndian.Uint16(value[12:14])
	// TS 23.003 19.4.2.3 reserves TAC 0000 and FFFE. ECI is exactly
	// 28 bits (not the 9-bit physical PCI); zero ECI/PCI need not be absent.
	if tac == 0 || tac == 0xfffe || eci > 0x0fffffff || pci > 503 {
		return nasLTEServingCell{}, ErrServingCellUnavailable
	}
	band, duplex, err := nasLTEDuplexFromEARFCN(earfcn)
	if err != nil {
		return nasLTEServingCell{}, err
	}
	for offset := header; offset < len(value); offset += measurement {
		if binary.LittleEndian.Uint16(value[offset:offset+2]) > 503 {
			return nasLTEServingCell{}, ErrServingCellUnavailable
		}
		// Measurements are signed i16 values and may include unavailable
		// sentinels. Neither signal strength nor list ordering elects a cell.
	}
	return nasLTEServingCell{cell: vowifi.LTECell{MCC: mcc, MNC: mnc, Duplex: duplex, TAC: tac, CellID: eci},
		earfcn: earfcn, pci: pci, band: band, idle: value[0] == 1}, nil
}

func decodeNASPLMN(value []byte) (string, string, bool) {
	if len(value) != 3 {
		return "", "", false
	}
	// TS 24.008 10.5.1.3 (LAI PLMN octets): MCC digit 2|1,
	// MNC digit 3|MCC digit 3, MNC digit 2|1. Only MNC digit 3 may
	// be 0xF, meaning exactly TWO digits; never turn it into a leading zero.
	digits := [6]byte{value[0] & 15, value[0] >> 4, value[1] & 15, value[2] & 15, value[2] >> 4, value[1] >> 4}
	for index, digit := range digits {
		if digit > 9 && !(index == 5 && digit == 15) {
			return "", "", false
		}
	}
	mcc := string([]byte{'0' + digits[0], '0' + digits[1], '0' + digits[2]})
	if mcc == "000" {
		return "", "", false
	}
	mnc := []byte{'0' + digits[3], '0' + digits[4]}
	if digits[5] != 15 {
		mnc = append(mnc, '0'+digits[5])
	}
	return mcc, string(mnc), true
}

// Only explicitly verified LTE DOWNLINK ranges from TS 36.101 V17.16.0
// Tables 5.5-1 and 5.7.3-1 are supported. These are numerical radio allocations,
// not guesses based on IMSI, geography, operator, or the modem's supported bands.
// https://www.etsi.org/deliver/etsi_ts/136100_136199/136101/17.16.00_60/ts_136101v171600p.pdf
// The lookup identifies duplex only; it does not validate channel-bandwidth
// edge restrictions, PCell/SCell role, RF attachment, or sample freshness.
func nasLTEDuplexFromEARFCN(earfcn uint32) (uint16, string, error) {
	type bandRange struct {
		low, high uint32
		band      uint16
	}
	for _, row := range [...]bandRange{
		{0, 599, 1}, {600, 1199, 2}, {1200, 1949, 3}, {1950, 2399, 4}, {2400, 2649, 5},
		// Band 6 (2650-2749) and 23 (7500-7699) are not applicable in
		// Table 5.5-1 note 1. Do not infer support from the numeric table alone.
		{2750, 3449, 7}, {3450, 3799, 8}, {3800, 4149, 9}, {4150, 4749, 10}, {4750, 4949, 11},
		{5010, 5179, 12}, {5180, 5279, 13}, {5280, 5379, 14}, {5730, 5849, 17}, {5850, 5999, 18},
		{6000, 6149, 19}, {6150, 6449, 20}, {6450, 6599, 21}, {6600, 7399, 22}, {7700, 8039, 24},
		{8040, 8689, 25}, {8690, 9039, 26}, {9040, 9209, 27}, {9210, 9659, 28},
		// Bands 29 (9660-9769) and 32 (9920-10359) are supplemental DL
		// only and cannot independently establish cellular IMS access duplex.
		{9770, 9869, 30}, {9870, 9919, 31},
	} {
		if earfcn >= row.low && earfcn <= row.high {
			return row.band, "FDD", nil
		}
	}
	for _, row := range [...]bandRange{
		{36000, 36199, 33}, {36200, 36349, 34}, {36350, 36949, 35}, {36950, 37549, 36}, {37550, 37749, 37},
		{37750, 38249, 38}, {38250, 38649, 39}, {38650, 39649, 40}, {39650, 41589, 41}, {41590, 43589, 42},
		{43590, 45589, 43}, {45590, 46589, 44}, {46590, 46789, 45}, {46790, 54539, 46}, {54540, 55239, 47},
		{55240, 56739, 48}, {56740, 58239, 49}, {58240, 59089, 50}, {59090, 59139, 51}, {59140, 60139, 52}, {60140, 60254, 53},
	} {
		if earfcn >= row.low && earfcn <= row.high {
			return row.band, "TDD", nil
		}
	}
	// Includes all unlisted/reserved gaps, FDD uplink EARFCNs, SDL, and
	// all extended bands >=65536. Never truncate or modulo an extended EARFCN.
	return 0, "", ErrServingCellUnavailable
}
