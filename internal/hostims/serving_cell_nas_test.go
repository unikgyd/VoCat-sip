package hostims

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"vocat/internal/vowifi"
)

func nasLTEGoldenValue() []byte {
	// Independent little-endian TLV vector: 310-260 / TAC ABCD /
	// ECI 1234567 / DL EARFCN 300 / PCI 42. Measurements are signed.
	value, err := hex.DecodeString("01130062cdab674523012c012a000508063e022a0088ff7cfca8fd1f004d00c0ff18fcd4fd1a00")
	if err != nil {
		panic(err)
	}
	return value
}

func nasLTEFrame(value []byte, extra ...qmi.TLV) []byte {
	tlvs := []qmi.TLV{{Type: 2, Value: []byte{0, 0, 0, 0}}, {Type: 0x13, Value: value}}
	tlvs = append(tlvs, extra...)
	return proxyResponse(&qmi.Packet{ServiceType: qmi.ServiceNAS, ClientID: 7, TransactionID: 0x1122, MessageID: nasGetCellLocationInfo}, tlvs)
}

func TestNASLTESingleResponseGoldenWireAndPANIFields(t *testing.T) {
	// Entire response is independent of the request encoder/fixture builder.
	frame, err := hex.DecodeString("013d00800307022211430031000204000000000013270001130062cdab674523012c012a000508063e022a0088ff7cfca8fd1f004d00c0ff18fcd4fd1a00")
	if err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), frame...)
	decoded, err := decodeNASLTECellLocationFrame(frame, 7, 0x1122)
	if err != nil {
		t.Fatal(err)
	}
	want := vowifi.LTECell{MCC: "310", MNC: "260", Duplex: "FDD", TAC: 0xabcd, CellID: 0x1234567}
	if decoded.cell != want || decoded.earfcn != 300 || decoded.pci != 42 || decoded.band != 1 || !decoded.idle {
		t.Fatal("serving header fields, endianness or duplex changed")
	}
	if pani, err := decoded.cell.PAccessNetworkInfo(); err != nil || pani != "3GPP-E-UTRAN-FDD;utran-cell-id-3gpp=310260ABCD1234567" {
		t.Fatal("wrong PANI field encoding")
	}
	if !bytes.Equal(frame, before) {
		t.Fatal("decoder modified input")
	}
	clear(frame)
	if decoded.cell != want {
		t.Fatal("decoded location aliases input")
	}
	for _, value := range []any{decoded, &decoded} {
		formatted := fmt.Sprintf("%v %+v %#v", value, value, value)
		for _, private := range []string{"310", "260", "ABCD", "1234567", "43981"} {
			if strings.Contains(formatted, private) {
				t.Fatal("candidate formatting leaked location")
			}
		}
	}
}

func TestNASLTEPLMNPackedBCDDoesNotGuessMNCDigitCount(t *testing.T) {
	for _, row := range []struct {
		packed   [3]byte
		mcc, mnc string
	}{
		{[3]byte{0x13, 0xf0, 0x62}, "310", "26"},
		{[3]byte{0x13, 0x00, 0x62}, "310", "260"},
		{[3]byte{0x00, 0xf1, 0x10}, "001", "01"},
		{[3]byte{0x00, 0x11, 0x00}, "001", "001"},
		{[3]byte{0x02, 0xf4, 0x40}, "204", "04"},
	} {
		mcc, mnc, ok := decodeNASPLMN(row.packed[:])
		if !ok || mcc != row.mcc || mnc != row.mnc {
			t.Fatal("BCD PLMN or MNC width changed")
		}
		value := nasLTEGoldenValue()
		copy(value[1:4], row.packed[:])
		decoded, err := decodeNASLTEIntraFrequency(value)
		if err != nil || decoded.cell.MCC != row.mcc || decoded.cell.MNC != row.mnc {
			t.Fatal("serving PLMN replaced by another identity")
		}
	}
	for nibble := 0; nibble < 6; nibble++ {
		for digit := byte(10); digit < 16; digit++ {
			if nibble == 3 && digit == 15 {
				continue
			} // The ONLY filler position.
			value := []byte{0x13, 0x00, 0x62}
			index, shift := nibble/2, uint(nibble%2*4)
			value[index] = value[index]&^(15<<shift) | digit<<shift
			if mcc, mnc, ok := decodeNASPLMN(value); ok || mcc != "" || mnc != "" {
				t.Fatal("invalid BCD accepted")
			}
		}
	}
	for _, value := range [][]byte{nil, {0, 0, 0}, {0x13, 0, 0x62, 0}, {0xff, 0xff, 0xff}} {
		if mcc, mnc, ok := decodeNASPLMN(value); ok || mcc != "" || mnc != "" {
			t.Fatal("unavailable PLMN accepted")
		}
	}
}

func TestNASLTEEARFCNFull16BitDomainAndUnsupportedExtendedBands(t *testing.T) {
	// Independent interval oracle: neither an uplink, an SDL-only band nor a
	// reserved/not-applicable gap is a blanket "EARFCN < 36000 means FDD".
	fdd := func(n uint32) bool {
		return n <= 2649 || n >= 2750 && n <= 4949 || n >= 5010 && n <= 5379 ||
			n >= 5730 && n <= 7399 || n >= 7700 && n <= 9659 || n >= 9770 && n <= 9919
	}
	for n := uint32(0); n <= 65536; n++ {
		band, duplex, err := nasLTEDuplexFromEARFCN(n)
		want := ""
		if fdd(n) {
			want = "FDD"
		} else if n >= 36000 && n <= 60254 {
			want = "TDD"
		}
		if want == "" {
			if band != 0 || duplex != "" || !errors.Is(err, ErrServingCellUnavailable) {
				t.Fatal("reserved/SDL/uplink/extended EARFCN accepted", n)
			}
		} else if err != nil || duplex != want || band == 0 || band > 53 || band == 6 || band == 23 || band == 29 || band == 32 {
			t.Fatal("valid duplex mapping lost", n)
		}
	}
	for _, row := range []struct {
		earfcn uint32
		band   uint16
		duplex string
	}{
		{0, 1, "FDD"}, {2649, 5, "FDD"}, {2750, 7, "FDD"}, {5010, 12, "FDD"}, {5379, 14, "FDD"},
		{5730, 17, "FDD"}, {7399, 22, "FDD"}, {7700, 24, "FDD"}, {9659, 28, "FDD"}, {9770, 30, "FDD"}, {9919, 31, "FDD"},
		{36000, 33, "TDD"}, {36349, 34, "TDD"}, {39650, 41, "TDD"}, {41589, 41, "TDD"}, {41590, 42, "TDD"},
		{46790, 46, "TDD"}, {55240, 48, "TDD"}, {58240, 50, "TDD"}, {60140, 53, "TDD"}, {60254, 53, "TDD"},
	} {
		band, duplex, err := nasLTEDuplexFromEARFCN(row.earfcn)
		if err != nil || band != row.band || duplex != row.duplex {
			t.Fatal("wrong allocation boundary", row.earfcn)
		}
	}
	for _, n := range []uint32{65537, 66435, 66436, 68586, 70655, 131072, 262143, ^uint32(0)} {
		if band, duplex, err := nasLTEDuplexFromEARFCN(n); band != 0 || duplex != "" || !errors.Is(err, ErrServingCellUnavailable) {
			t.Fatal("extended EARFCN was wrapped or guessed")
		}
	}
}

func TestNASLTEIntraFrequencyStrictShapeAndIdentityBounds(t *testing.T) {
	valid := nasLTEGoldenValue()
	for end := 0; end < len(valid); end++ {
		if cell, err := decodeNASLTEIntraFrequency(valid[:end]); cell != (nasLTEServingCell{}) || !errors.Is(err, ErrServingCellUnavailable) {
			t.Fatal("truncated LTE TLV accepted", end)
		}
	}
	for _, mode := range []string{"trailing", "extended-earfcn-layout", "count", "idle", "reserved-tac-zero", "reserved-tac-fffe", "eci-wide", "eci-sentinel", "pci", "neighbor-pci", "bad-plmn", "reserved-earfcn", "sdl-earfcn", "uplink-earfcn"} {
		t.Run(mode, func(t *testing.T) {
			value := nasLTEGoldenValue()
			switch mode {
			case "trailing":
				value = append(value, 0)
			case "extended-earfcn-layout":
				value = append(append(append([]byte(nil), value[:12]...), 1, 0), value[12:]...)
			case "count":
				value[18]++
			case "idle":
				value[0] = 2
			case "reserved-tac-zero":
				binary.LittleEndian.PutUint16(value[4:6], 0)
			case "reserved-tac-fffe":
				binary.LittleEndian.PutUint16(value[4:6], 0xfffe)
			case "eci-wide":
				binary.LittleEndian.PutUint32(value[6:10], 0x10000000)
			case "eci-sentinel":
				binary.LittleEndian.PutUint32(value[6:10], 0xffffffff)
			case "pci":
				binary.LittleEndian.PutUint16(value[12:14], 504)
			case "neighbor-pci":
				binary.LittleEndian.PutUint16(value[19:21], 504)
			case "bad-plmn":
				value[1] = 0xff
			case "reserved-earfcn":
				binary.LittleEndian.PutUint16(value[10:12], 4950)
			case "sdl-earfcn":
				binary.LittleEndian.PutUint16(value[10:12], 9660)
			case "uplink-earfcn":
				binary.LittleEndian.PutUint16(value[10:12], 18000)
			}
			if cell, err := decodeNASLTEIntraFrequency(value); cell != (nasLTEServingCell{}) || !errors.Is(err, ErrServingCellUnavailable) {
				t.Fatal("invalid LTE TLV exposed partial data", err)
			}
		})
	}
}

func TestNASLTECandidateUsesServingHeaderNotNeighborOrdering(t *testing.T) {
	for _, count := range []int{0, 1, 2, 255} {
		value := append([]byte(nil), nasLTEGoldenValue()[:19]...)
		value[0], value[18] = 0, byte(count)
		binary.LittleEndian.PutUint16(value[4:6], 0xffff) // Not a TS23.003 reserved TAC.
		binary.LittleEndian.PutUint32(value[6:10], 0)
		binary.LittleEndian.PutUint16(value[10:12], 39650)
		binary.LittleEndian.PutUint16(value[12:14], 503)
		for i := 0; i < count; i++ {
			measurement := make([]byte, 10)
			binary.LittleEndian.PutUint16(measurement, uint16(i)) // No PCI503 entry.
			for j := 2; j < 10; j++ {
				measurement[j] = 0xff
			}
			value = append(value, measurement...)
		}
		decoded, err := decodeNASLTEIntraFrequency(value)
		if err != nil || decoded.cell.CellID != 0 || decoded.pci != 503 || decoded.cell.TAC != 0xffff || decoded.idle || decoded.cell.Duplex != "TDD" || decoded.band != 41 {
			t.Fatal("neighbor measurement elected a serving cell or ignored valid zero identities", err)
		}
	}
}

func TestNASLTEFrameRejectsEnvelopeFailuresAndUnknownOverlays(t *testing.T) {
	for _, mode := range []string{"request", "indication", "service", "cid", "transaction", "message", "qmux-length", "service-length", "trailer", "duplicate-result", "duplicate-lte", "missing-result", "failed-result", "inconsistent-result", "short-result", "missing-lte", "unknown-extension", "nr-info", "neighbor-lte-only", "short-timing"} {
		t.Run(mode, func(t *testing.T) {
			frame := nasLTEFrame(nasLTEGoldenValue())
			switch mode {
			case "request":
				frame[6] = 0
			case "indication":
				frame[6] = 4
			case "service":
				frame[4] = qmi.ServiceWDS
			case "cid":
				frame[5]++
			case "transaction":
				frame[7]++
			case "message":
				frame[9]++
			case "qmux-length":
				frame[1]++
			case "service-length":
				frame[11]++
			case "trailer":
				frame = append(frame, 0)
			case "duplicate-result":
				frame = nasLTEFrame(nasLTEGoldenValue(), qmi.TLV{Type: 2, Value: []byte{0, 0, 0, 0}})
			case "duplicate-lte":
				frame = nasLTEFrame(nasLTEGoldenValue(), qmi.TLV{Type: 0x13, Value: nasLTEGoldenValue()})
			case "missing-result":
				frame[13] = 0x14
			case "failed-result":
				frame[16], frame[18] = 1, 14
			case "inconsistent-result":
				frame[18] = 14
			case "short-result":
				frame[14] = 3
			case "missing-lte":
				frame[20] = 0x14
			case "unknown-extension":
				frame = nasLTEFrame(nasLTEGoldenValue(), qmi.TLV{Type: 0x27, Value: []byte{1, 0, 1, 0}})
			case "nr-info":
				frame = nasLTEFrame(nasLTEGoldenValue(), qmi.TLV{Type: 0x2f, Value: make([]byte, 24)})
			case "neighbor-lte-only":
				frame[20] = 0x18
			case "short-timing":
				frame = nasLTEFrame(nasLTEGoldenValue(), qmi.TLV{Type: 0x1e, Value: []byte{1, 2, 3}})
			}
			if cell, err := decodeNASLTECellLocationFrame(frame, 7, 0x1122); cell != (nasLTEServingCell{}) || !errors.Is(err, ErrServingCellUnavailable) {
				t.Fatal("invalid/unmatched response exposed cell", err)
			}
		})
	}
	for _, row := range []struct {
		cid byte
		tx  uint16
	}{{0, 1}, {255, 1}, {7, 0}} {
		if cell, err := decodeNASLTECellLocationFrame(nasLTEFrame(nasLTEGoldenValue()), row.cid, row.tx); cell != (nasLTEServingCell{}) || !errors.Is(err, ErrServingCellUnavailable) {
			t.Fatal("unowned/unspecified transaction accepted")
		}
	}
}

func TestNASLTENeighborDataCannotSupplyDuplexOrUpgradeCachedSource(t *testing.T) {
	frame := nasLTEFrame(nasLTEGoldenValue(), qmi.TLV{Type: 0x14, Value: []byte{1, 0}},
		qmi.TLV{Type: 0x15, Value: []byte{1, 0}}, qmi.TLV{Type: 0x16, Value: []byte{1, 0}}, qmi.TLV{Type: 0x1e, Value: make([]byte, 4)})
	decoded, err := decodeNASLTECellLocationFrame(frame, 7, 0x1122)
	if err != nil || decoded.cell.Duplex != "FDD" {
		t.Fatal("unrelated neighbor data affected serving duplex", err)
	}
	source, _ := servingCellFixture(t)
	if cell, err := source.Read(context.Background()); cell != (vowifi.LTECell{}) || !errors.Is(err, ErrServingCellUnavailable) {
		t.Fatal("standalone NAS decoder upgraded cached source")
	}
}

func FuzzNASLTESingleResponseDecoder(f *testing.F) {
	f.Add(nasLTEFrame(nasLTEGoldenValue()))
	tdd := nasLTEGoldenValue()
	binary.LittleEndian.PutUint16(tdd[10:12], 39650)
	f.Add(nasLTEFrame(tdd))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		before := append([]byte(nil), raw...)
		decoded, err := decodeNASLTECellLocationFrame(raw, 7, 0x1122)
		if !bytes.Equal(raw, before) {
			t.Fatal("decoder modified input")
		}
		if err != nil {
			if decoded != (nasLTEServingCell{}) || !errors.Is(err, ErrServingCellUnavailable) {
				t.Fatal("invalid response exposed partial/private data")
			}
			return
		}
		cell := decoded.cell
		band, duplex, lookupErr := nasLTEDuplexFromEARFCN(decoded.earfcn)
		if lookupErr != nil || decoded.band != band || cell.Duplex != duplex || !asciiDigits(cell.MCC, 3, 3) || cell.MCC == "000" ||
			!asciiDigits(cell.MNC, 2, 3) || cell.TAC == 0 || cell.TAC == 0xfffe || cell.CellID > 0xfffffff || decoded.pci > 503 {
			t.Fatal("invalid decoded candidate")
		}
		if _, err := cell.PAccessNetworkInfo(); err != nil {
			t.Fatal("candidate has invalid PANI fields")
		}
	})
}
