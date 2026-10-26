package hostims

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"vocat/internal/vowifi"
)

// Session methods never enable RF, enter a PIN, change slots/profiles, or use
// the basic APDU channel. The opener must prove the selected physical endpoint.
type uimSession interface {
	Card(context.Context, cardBinding) (uimCard, string, error)
	Open(context.Context, byte, []byte) (byte, error)
	APDU(context.Context, byte, byte, []byte) ([]byte, error)
	CloseChannel(context.Context, byte, byte) error
	Uncertain() bool
	Close() error
}
type uimOpener func(context.Context, Selection) (uimSession, error)

type CardIdentity struct {
	subscriber                           vowifi.SIMIdentity
	domain, private, public, application string
	aid                                  []byte
	slot                                 byte
}

func (c CardIdentity) Subscriber() vowifi.SIMIdentity {
	subscriber := c.subscriber
	subscriber.DiscoveryPLMNs = append([]string(nil), subscriber.DiscoveryPLMNs...)
	return subscriber
}
func (c CardIdentity) IMSDomain() string       { return c.domain }
func (c CardIdentity) PrivateIdentity() string { return c.private }
func (c CardIdentity) PublicIdentity() string  { return c.public }
func (c CardIdentity) Application() string     { return c.application }
func (CardIdentity) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private IMS card identity>")
}

func channelCLA(channel byte) (byte, error) {
	if channel < 1 || channel > 19 {
		return 0, ErrSIMApplication
	}
	if channel < 4 {
		return channel, nil
	}
	return 0x40 | (channel - 4), nil
}

// Only idempotent SELECT/READ may correct Le. AUTHENTICATE is NEVER replayed
// on 6Cxx or a transport failure. 61xx/9Fxx fetch the existing response only.
func exchangeCard(ctx context.Context, session uimSession, slot, channel byte, command []byte) ([]byte, error) {
	cla, err := channelCLA(channel)
	if err != nil || len(command) < 4 {
		return nil, ErrSIMApplication
	}
	request := append([]byte(nil), command...)
	request[0] = cla
	defer func() { clear(request) }()
	response, err := session.APDU(ctx, slot, channel, request)
	if err != nil {
		return nil, err
	}
	if len(response) == 2 && response[0] == 0x6c && (request[1] == 0xa4 || request[1] == 0xb0 || request[1] == 0xb2) {
		if len(request) == 5 {
			request[4] = response[1]
		} else if request[1] == 0xa4 {
			request = append(request, response[1])
		} else {
			return nil, ErrSIM
		}
		clear(response)
		response, err = session.APDU(ctx, slot, channel, request)
		if err != nil {
			return nil, err
		}
	}
	var data []byte
	for count := 0; count < 8; count++ {
		if len(response) < 2 || len(data)+len(response) > 8192 {
			clear(response)
			clear(data)
			return nil, ErrSIM
		}
		data = append(data, response[:len(response)-2]...)
		sw1, sw2 := response[len(response)-2], response[len(response)-1]
		clear(response)
		if sw1 != 0x61 && sw1 != 0x9f {
			return append(data, sw1, sw2), nil
		}
		response, err = session.APDU(ctx, slot, channel, []byte{cla, 0xc0, 0, 0, sw2})
		if err != nil {
			clear(data)
			return nil, err
		}
	}
	clear(response)
	clear(data)
	return nil, ErrSIM
}

func readCardFile(ctx context.Context, session uimSession, slot, channel byte, file uint16, record bool) ([]byte, error) {
	selected, err := exchangeCard(ctx, session, slot, channel, []byte{0, 0xa4, 0, 4, 2, byte(file >> 8), byte(file)})
	if err != nil {
		return nil, err
	}
	if len(selected) == 2 && selected[0] == 0x6a && (selected[1] == 0x86 || selected[1] == 0x81) {
		clear(selected)
		selected, err = exchangeCard(ctx, session, slot, channel, []byte{0, 0xa4, 0, 0x0c, 2, byte(file >> 8), byte(file)})
		if err != nil {
			return nil, err
		}
	}
	if !cardSuccess(selected) {
		clear(selected)
		return nil, ErrSIMIdentity
	}
	clear(selected)
	command := []byte{0, 0xb0, 0, 0, 0}
	if record {
		command = []byte{0, 0xb2, 1, 4, 0}
	}
	response, err := exchangeCard(ctx, session, slot, channel, command)
	if err != nil {
		return nil, err
	}
	if !cardSuccess(response) {
		clear(response)
		return nil, ErrSIMIdentity
	}
	result := append([]byte(nil), response[:len(response)-2]...)
	clear(response)
	return result, nil
}
func cardSuccess(raw []byte) bool {
	return len(raw) >= 2 && raw[len(raw)-2] == 0x90 && raw[len(raw)-1] == 0
}

type cardTransaction struct {
	provider *SIMProvider
	session  uimSession
	card     uimCard
	channel  byte
	unsafe   bool
}

func (t *cardTransaction) verify(ctx context.Context) error {
	if err := t.provider.verifyLease(ctx); err != nil {
		t.unsafe = true
		return err
	}
	card, iccid, err := t.session.Card(ctx, t.provider.lease.selection.snapshot.Binding)
	if err != nil || iccid != t.provider.lease.selection.snapshot.Binding.ICCID || card.Slot != t.card.Slot ||
		!bytes.Equal(card.USIM, t.card.USIM) || card.HasISIM != t.card.HasISIM || !bytes.Equal(card.ISIM, t.card.ISIM) {
		t.unsafe = true
		return ErrSIM
	}
	return nil
}
func (t *cardTransaction) open(ctx context.Context, aid []byte) error {
	if t.channel != 0 {
		return ErrBusy
	}
	if err := t.verify(ctx); err != nil {
		return err
	}
	channel, err := t.session.Open(ctx, t.card.Slot, aid)
	if err != nil {
		if t.session.Uncertain() {
			t.unsafe = true
			return ErrUncertain
		}
		return err
	}
	t.channel = channel
	if _, err := channelCLA(channel); err != nil {
		t.unsafe = true
		return ErrUncertain
	}
	return nil
}
func (t *cardTransaction) closeChannel(ctx context.Context) error {
	if t.channel == 0 {
		return nil
	}
	if t.unsafe || t.session.Uncertain() {
		return ErrUncertain
	}
	if err := t.verify(ctx); err != nil {
		return err
	}
	if err := t.session.CloseChannel(ctx, t.card.Slot, t.channel); err != nil {
		t.unsafe = true
		return ErrUncertain
	}
	t.channel = 0
	return nil
}
func (t *cardTransaction) readIdentity(ctx context.Context) (CardIdentity, error) {
	binding := t.provider.lease.selection.snapshot.Binding
	if err := t.open(ctx, t.card.USIM); err != nil {
		return CardIdentity{}, err
	}
	imsiRaw, err := readCardFile(ctx, t.session, t.card.Slot, t.channel, 0x6f07, false)
	if err != nil {
		return CardIdentity{}, err
	}
	imsi, err := decodeCardIMSI(imsiRaw)
	clear(imsiRaw)
	if err != nil || imsi != binding.IMSI {
		t.unsafe = true
		return CardIdentity{}, ErrSIM
	}
	ad, adErr := readCardFile(ctx, t.session, t.card.Slot, t.channel, 0x6fad, false)
	mncLength := 0
	if adErr == nil && len(ad) >= 4 {
		mncLength = int(ad[3] & 15)
	}
	clear(ad)
	if adErr != nil && t.session.Uncertain() {
		return CardIdentity{}, ErrUncertain
	}
	if adErr != nil && adErr != ErrSIMIdentity {
		return CardIdentity{}, adErr
	}
	if err := t.closeChannel(ctx); err != nil {
		return CardIdentity{}, err
	}
	identity := CardIdentity{subscriber: vowifi.SIMIdentity{ICCID: binding.ICCID, IMSI: imsi, IMEI: binding.Equipment, HomeMCC: imsi[:3]}, slot: t.card.Slot}
	if mncLength == 2 || mncLength == 3 {
		identity.subscriber.HomeMNC = imsi[3 : 3+mncLength]
	}
	if t.card.HasISIM {
		if err := t.open(ctx, t.card.ISIM); err != nil {
			return CardIdentity{}, err
		}
		values := make([]string, 3)
		for i, file := range []uint16{0x6f02, 0x6f03, 0x6f04} {
			raw, err := readCardFile(ctx, t.session, t.card.Slot, t.channel, file, i == 2)
			if err != nil {
				return CardIdentity{}, err
			}
			values[i], err = decodeISIMText(raw)
			clear(raw)
			if err != nil {
				return CardIdentity{}, err
			}
		}
		if !validPrivateIMSIdentity(values[0]) || !validDomain(values[1]) || !validPublicIMSIdentity(values[2]) {
			return CardIdentity{}, ErrSIMIdentity
		}
		identity.private, identity.domain, identity.public = values[0], strings.ToLower(values[1]), values[2]
		identity.application = "ISIM"
		identity.aid = append([]byte(nil), t.card.ISIM...)
	} else {
		if mncLength != 2 && mncLength != 3 {
			return CardIdentity{}, ErrSIMIdentity
		}
		mnc := identity.subscriber.HomeMNC
		if len(mnc) == 2 {
			mnc = "0" + mnc
		}
		identity.domain = "ims.mnc" + mnc + ".mcc" + identity.subscriber.HomeMCC + ".3gppnetwork.org"
		identity.private = imsi + "@" + identity.domain
		identity.public = "sip:" + identity.private
		identity.application = "USIM"
		identity.aid = append([]byte(nil), t.card.USIM...)
	}
	if err := t.closeChannel(ctx); err != nil {
		return CardIdentity{}, err
	}
	return identity, nil
}

func (t *cardTransaction) authenticate(ctx context.Context, identity CardIdentity, challenge vowifi.AKAChallenge, caller context.Context) (vowifi.AKAResult, error) {
	// Provisioning can change without an ICCID/AID change. Re-read all identity
	// files in the serialized transaction, not just the USIM IMSI.
	current, err := t.readIdentity(ctx)
	if err != nil {
		return vowifi.AKAResult{}, err
	}
	defer clear(current.aid)
	if !sameCardIdentity(current, identity) {
		t.unsafe = true
		return vowifi.AKAResult{}, ErrSIM
	}
	if identity.slot != t.card.Slot || identity.application == "ISIM" && !bytes.Equal(identity.aid, t.card.ISIM) || identity.application == "USIM" && !bytes.Equal(identity.aid, t.card.USIM) {
		return vowifi.AKAResult{}, ErrSIM
	}
	if err := t.open(ctx, identity.aid); err != nil {
		return vowifi.AKAResult{}, err
	}
	if caller.Err() != nil {
		return vowifi.AKAResult{}, caller.Err()
	}
	if err := t.provider.issueChallenge(ctx, challenge); err != nil {
		return vowifi.AKAResult{}, err
	}
	if caller.Err() != nil {
		return vowifi.AKAResult{}, caller.Err()
	}
	command := []byte{0, 0x88, 0, 0x81, 34, 16}
	command = append(command, challenge.RAND[:]...)
	command = append(command, 16)
	command = append(command, challenge.AUTN[:]...)
	command = append(command, 0)
	raw, err := exchangeCard(ctx, t.session, t.card.Slot, t.channel, command)
	clear(command)
	if err != nil {
		return vowifi.AKAResult{}, err
	}
	defer clear(raw)
	result, err := parseCardAKA(raw)
	if err != nil {
		return vowifi.AKAResult{}, err
	}
	if err := t.verify(ctx); err != nil {
		clearCardAKA(&result)
		return vowifi.AKAResult{}, err
	}
	return result, nil
}
