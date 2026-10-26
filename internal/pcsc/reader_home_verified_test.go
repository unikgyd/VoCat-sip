package pcsc

import (
	"context"
	"errors"
	"testing"
)

type readerHomePINBackend struct{ card Card }

func (b readerHomePINBackend) Readers(context.Context) ([]Reader, error)    { return nil, nil }
func (b readerHomePINBackend) Open(context.Context, Selector) (Card, error) { return b.card, nil }

func TestReaderHomePINMetadataRemainsAvailableWithoutSubmittingPIN(t *testing.T) {
	card := &scriptedCard{replies: []scriptedReply{
		{sw: 0x9000}, {sw: 0x9000}, {data: []byte{0x98, 0x10, 0x32, 0x54, 0x76, 0x98, 0x10, 0x32, 0x54, 0xF6}, sw: 0x9000},
		{sw: 0x9000}, {sw: 0x9000}, {data: []byte{0x61, 9, 0x4F, 7, 0xA0, 0, 0, 0, 0x87, 0x10, 2}, sw: 0x9000}, {sw: 0x6A83}, {sw: 0x9000},
		{sw: 0x63C1},
	}}
	s := NewWithBackend(readerHomePINBackend{card: card})
	id, err := s.ReadIdentity(context.Background(), Selector{USBPath: "synthetic"}, "1234")
	if !errors.Is(err, ErrPINTriesLow) || !id.PINRequired || id.PINTries != 1 {
		t.Fatalf("PIN UI evidence lost: %+v %v", id, err)
	}
	for _, cmd := range card.calls {
		if len(cmd) > 5 && cmd[1] == 0x20 {
			t.Fatal("low-tries query submitted a PIN")
		}
	}
}
