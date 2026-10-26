package rtpevent

import (
	"context"
	"errors"
	"testing"
)

func TestTelephoneEventMaskAndBoundedReceipt(t *testing.T) {
	for _, value := range []string{"", "+1", "0-99999", "15-0", "0-15;injected=1", "1,,2", "1-2-3", "-1", "256"} {
		if _, ok := ParseMask(value); ok {
			t.Fatal("invalid event set", value)
		}
	}
	mask, ok := ParseMask("0-9,11,255")
	if !ok || Digits(mask) != "0123456789#" {
		t.Fatal(mask, ok)
	}
	spec := Spec{Payload: 110, Mask: mask}
	var receiver Receiver
	for _, payload := range [][]byte{{5, 0x0a, 0, 160}, {5, 0x8a, 0, 0}, {10, 0x8a, 0, 160}, {5, 0x8a, 0, 160, 0}} {
		if _, ok := receiver.Accept(spec, payload, 1, 2); ok {
			t.Fatal("invalid or unfinished event delivered", payload)
		}
	}
	for i := 0; i < 3; i++ {
		digit, ok := receiver.Accept(spec, []byte{5, 0x8a, 5, 0}, 1, 2)
		if ok != (i == 0) || ok && digit != '5' {
			t.Fatal("redundant final was delivered twice", i, digit, ok)
		}
	}
	for i := 0; i < 1000; i++ {
		receiver.Accept(spec, []byte{5, 0x8a, 5, 0}, 1, uint32(i+3))
	}
	if len(receiver.order) > 64 || len(receiver.seen) > 64 {
		t.Fatal("receipt cache unbounded")
	}
}
func TestTelephoneEventTransmitDoesNotRetryAnUnknownWrite(t *testing.T) {
	writes := 0
	unknown := errors.New("wire outcome unknown")
	err := Transmit(context.Background(), nil, 5, func([]byte, bool) error { writes++; return unknown })
	if !errors.Is(err, unknown) || writes != 1 {
		t.Fatal(err, writes)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = Transmit(ctx, nil, 5, func([]byte, bool) error { t.Fatal("canceled event wrote"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
