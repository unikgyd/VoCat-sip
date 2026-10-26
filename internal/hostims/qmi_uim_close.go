package hostims

import (
	"bytes"
	"encoding/binary"
	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"time"
)

type ownedUIMClose struct {
	cid, slot, channel byte
	aid                []byte
	expires            time.Time
}

// Register only immediately before closing a channel owned by this UIM
// session. A matching asynchronous Session Closed is not a SIM epoch change,
// and never substitutes for the solicited Close or CID-release ACK.
func (p *proxyQMI) expectOwnedUIMClose(cid, slot, channel byte, aid []byte) error {
	if cid == 0 || cid == 255 || slot < 1 || slot > 5 || len(aid) < 7 || len(aid) > 32 {
		return ErrSIMApplication
	}
	if _, err := channelCLA(channel); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken || p.closed.Load() {
		return ErrTransport
	}
	now := time.Now()
	live := p.uimCloses[:0]
	for _, notice := range p.uimCloses {
		if now.Before(notice.expires) {
			live = append(live, notice)
		}
	}
	p.uimCloses = live
	if len(live) >= 4 {
		return ErrBusy
	}
	p.uimCloses = append(p.uimCloses, ownedUIMClose{cid: cid, slot: slot, channel: channel, aid: append([]byte(nil), aid...), expires: now.Add(5 * time.Second)})
	return nil
}

func (p *proxyQMI) consumeOwnedUIMClose(packet *qmi.Packet) bool {
	if packet == nil || !packet.IsIndication || packet.ServiceType != qmi.ServiceUIM || packet.MessageID != 0x0043 || len(packet.TLVs) < 2 || len(packet.TLVs) > 4 {
		return false
	}
	var slot, channel byte
	var aid []byte
	seen := map[byte]bool{}
	for _, tlv := range packet.TLVs {
		if seen[tlv.Type] {
			return false
		}
		seen[tlv.Type] = true
		switch tlv.Type {
		case 1:
			if len(tlv.Value) != 1 {
				return false
			}
			slot = tlv.Value[0]
		case 0x11:
			if len(tlv.Value) != 1 {
				return false
			}
			channel = tlv.Value[0]
		case 0x10:
			if len(tlv.Value) < 8 || len(tlv.Value) > 33 || int(tlv.Value[0]) != len(tlv.Value)-1 {
				return false
			}
			aid = tlv.Value[1:]
		case 0x13:
			// QDC507's observed supplementary scalar. It cannot elect a channel,
			// supply identity, or turn an unknown UIM event into an expected close.
			if len(tlv.Value) != 4 || binary.LittleEndian.Uint32(tlv.Value) != 1 {
				return false
			}
		default:
			return false
		}
	}
	if !seen[1] || !seen[0x11] {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, notice := range p.uimCloses {
		if time.Now().Before(notice.expires) && notice.cid == packet.ClientID && notice.slot == slot && notice.channel == channel && (aid == nil || bytes.Equal(aid, notice.aid)) {
			p.uimCloses = append(p.uimCloses[:i], p.uimCloses[i+1:]...)
			return true
		}
	}
	return false
}
