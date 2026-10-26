package hostims

import (
	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"testing"
)

func TestNASLTEKnownExtensionsPreserveTheOriginalServingCell(t *testing.T) {
	base, err := decodeNASLTECellLocationFrame(nasLTEFrame(nasLTEGoldenValue()), 7, 0x1122)
	if err != nil {
		t.Fatal(err)
	}
	frame := nasLTEFrame(nasLTEGoldenValue(), qmi.TLV{Type: 0x26, Value: []byte{0xff, 0xff}}, qmi.TLV{Type: 0x27, Value: []byte{0x2c, 1, 0, 0}}, qmi.TLV{Type: 0x28, Value: []byte{0}})
	cell, err := decodeNASLTECellLocationFrame(frame, 7, 0x1122)
	if err != nil || cell != base {
		t.Fatal("documented optional fields changed or blocked the serving header", err)
	}
}

func TestNASLTEExtensionsCannotWrapOrReplaceTheServingChannel(t *testing.T) {
	for _, extra := range []qmi.TLV{
		{Type: 0x26, Value: []byte{0x91, 1}},
		{Type: 0x26, Value: []byte{0}},
		{Type: 0x27, Value: []byte{0x2d, 1, 0, 0}},
		{Type: 0x27, Value: []byte{0x2c, 1, 1, 0}},
		{Type: 0x27, Value: []byte{0x2c, 1, 0}},
		{Type: 0x28, Value: []byte{1}},
	} {
		if _, err := decodeNASLTECellLocationFrame(nasLTEFrame(nasLTEGoldenValue(), extra), 7, 0x1122); err == nil {
			t.Fatal("malformed or contradictory extension was accepted")
		}
	}
}
