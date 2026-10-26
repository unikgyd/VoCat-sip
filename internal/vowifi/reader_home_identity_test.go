package vowifi

import (
	"context"
	"errors"
	"testing"

	"vocat/internal/pcsc"
)

// This card interprets the actual read-only APDUs, rather than allowing the
// test to call an unimplemented future helper. No physical reader is opened.
type readerHomeCard struct {
	file  uint16
	calls int
}

func (c *readerHomeCard) Close() error { return nil }
func (c *readerHomeCard) Transmit(_ context.Context, apdu []byte) ([]byte, uint16, error) {
	c.calls++
	if len(apdu) < 4 {
		return nil, 0, errors.New("short APDU")
	}
	switch apdu[1] {
	case 0xA4:
		if apdu[2] == 4 {
			c.file = 0
			return nil, 0x9000, nil
		}
		if len(apdu) < 7 {
			return nil, 0, errors.New("short SELECT")
		}
		c.file = uint16(apdu[5])<<8 | uint16(apdu[6])
		if c.file == 0x6F46 || c.file == 0x6F42 {
			return nil, 0x6A82, nil
		}
		return nil, 0x9000, nil
	case 0x20:
		return nil, 0x9000, nil
	case 0xB2:
		if c.file == 0x2F00 && apdu[2] == 1 {
			return []byte{0x61, 9, 0x4F, 7, 0xA0, 0, 0, 0, 0x87, 0x10, 2}, 0x9000, nil
		}
		return nil, 0x6A83, nil
	case 0xB0:
		switch c.file {
		case 0x2FE2:
			return []byte{0x98, 0x10, 0x32, 0x54, 0x76, 0x98, 0x10, 0x32, 0x54, 0xF6}, 0x9000, nil
		case 0x6F07:
			return []byte{8, 0x99, 0x99, 0x21, 0x13, 0x32, 0x54, 0x76, 0x98}, 0x9000, nil
		case 0x6FAD:
			return []byte{0, 0, 0, 0xFF}, 0x9000, nil
		}
	}
	return nil, 0, errors.New("unexpected APDU")
}

func TestReaderHomePCSCFailureDoesNotPublishUnusableBinding(t *testing.T) {
	c := &readerHomeCard{}
	s := pcsc.NewWithBackend(&mockPCSCBackend{card: c})
	a, err := NewPCSCAdapter(s, func(context.Context, string) (pcsc.Selector, string, error) {
		return pcsc.Selector{USBPath: "1-1"}, "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ReadIdentity(context.Background(), "reader"); !errors.Is(err, ErrEC20MNCUnavailable) {
		t.Fatalf("expected unknown MNC: %v", err)
	}
	if len(a.bindings) != 0 {
		t.Fatal("unusable PC/SC identity published before home validation")
	}
}
