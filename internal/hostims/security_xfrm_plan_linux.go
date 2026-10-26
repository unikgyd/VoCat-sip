//go:build linux

package hostims

import (
	"fmt"
	"io"
	"net/netip"

	"vocat/internal/vowifi/imssec"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// Keys are safe to persist. Neither a key nor a matching fingerprint proves
// ownership; the journal must independently retain the successful ADD ACK.
type scopedXFRMKey struct {
	Kind             string
	Src, Dst         netip.Addr
	SPI              uint32
	Protocol         uint8
	SrcPort, DstPort uint16
	Direction        uint8
	Mark             uint32
}

type scopedXFRMOperation struct {
	Key         scopedXFRMKey
	Fingerprint string // normalized desired shape, policy auto-index excluded
	wireAdd     []byte // sensitive algorithm key bytes; never serialize/log
}

func (scopedXFRMOperation) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private scoped XFRM operation>")
}

type scopedXFRMPlan struct{ Operations []scopedXFRMOperation }

func (p *scopedXFRMPlan) Clear() {
	if p == nil {
		return
	}
	for i := range p.Operations {
		clear(p.Operations[i].wireAdd)
		p.Operations[i].wireAdd = nil
	}
}

func validateScopedXFRMKey(k scopedXFRMKey) error {
	validIP := func(ip netip.Addr) bool {
		return ip.IsValid() && !ip.Is4In6() && ip.Zone() == "" && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalUnicast()
	}
	if !validIP(k.Src) || !validIP(k.Dst) || k.Src.Is4() != k.Dst.Is4() || k.Src == k.Dst || k.Mark == 0 && k.Kind != "state" {
		return ErrXFRMSchema
	}
	switch k.Kind {
	case "state":
		if k.SPI == 0 || k.Protocol != unix.IPPROTO_ESP || k.SrcPort != 0 || k.DstPort != 0 || k.Direction != 0 {
			return ErrXFRMSchema
		}
	case "policy":
		if k.SPI != 0 || (k.Protocol != unix.IPPROTO_TCP && k.Protocol != unix.IPPROTO_UDP) || k.Direction > 1 ||
			(k.SrcPort != 0 && !imssec.ValidProtectedPort(int(k.SrcPort))) ||
			(k.DstPort != 0 && !imssec.ValidProtectedPort(int(k.DstPort))) || (k.SrcPort == 0 && k.DstPort == 0) {
			return ErrXFRMSchema
		}
	default:
		return ErrXFRMSchema
	}
	return nil
}

func scopedXFRMFamily(ip netip.Addr) uint16 {
	if ip.Is4() {
		return unix.AF_INET
	}
	return unix.AF_INET6
}
func scopedXFRMAddress(ip netip.Addr) (out nl.XfrmAddress) { copy(out[:], ip.AsSlice()); return }
func scopedXFRMSelector(k scopedXFRMKey) nl.XfrmSelector {
	bits := uint8(k.Src.BitLen())
	sel := nl.XfrmSelector{Saddr: scopedXFRMAddress(k.Src), Daddr: scopedXFRMAddress(k.Dst),
		Family: scopedXFRMFamily(k.Src), PrefixlenS: bits, PrefixlenD: bits, Proto: k.Protocol,
		Sport: nl.Swap16(k.SrcPort), Dport: nl.Swap16(k.DstPort)}
	if k.SrcPort != 0 {
		sel.SportMask = 0xffff
	}
	if k.DstPort != 0 {
		sel.DportMask = 0xffff
	}
	return sel
}

func scopedXFRMMark(mark uint32) []byte {
	return (&nl.XfrmMark{Value: mark, Mask: 0xffffffff}).Serialize()
}
func scopedXFRMAttr(dst []byte, kind uint16, payload []byte) []byte {
	// Write directly: NewRtAttr.Serialize followed by append would leave an
	// untracked temporary copy of algorithm key material in the Go heap.
	start, length := len(dst), 4+len(payload)
	aligned := (length + 3) &^ 3
	dst = append(dst, make([]byte, aligned)...)
	nl.NativeEndian().PutUint16(dst[start:], uint16(length))
	nl.NativeEndian().PutUint16(dst[start+2:], kind)
	copy(dst[start+4:], payload)
	return dst
}
func scopedXFRMLifetime() nl.XfrmLifetimeCfg {
	return nl.XfrmLifetimeCfg{SoftByteLimit: ^uint64(0), HardByteLimit: ^uint64(0), SoftPacketLimit: ^uint64(0), HardPacketLimit: ^uint64(0)}
}

func buildScopedXFRMPlan(config imssec.Config, mark, clientReqID, serverReqID uint32) (*scopedXFRMPlan, error) {
	return buildScopedXFRMPlanMode(config, mark, clientReqID, serverReqID, false)
}

// Linux socket policies use policy mark zero to resolve their required SA,
// independently of SO_MARK. Keep the original private mark on inbound SAs
// and global policies; outbound SAs are selected by the exact endpoint/ReqID
// required by each socket. A conflicting outbound SPI is rejected, never
// adopted. The durable mode bit preserves cleanup of older marked journals.
func buildScopedXFRMPlanMode(config imssec.Config, mark, clientReqID, serverReqID uint32, socketOutbound bool) (*scopedXFRMPlan, error) {
	plan, err := imssec.BuildScopedPlan(config, clientReqID, serverReqID)
	if err != nil || mark == 0 {
		return nil, ErrXFRMSchema
	}
	out := &scopedXFRMPlan{}
	success := false
	defer func() {
		if !success {
			out.Clear()
		}
	}()
	for ordinal, state := range plan.States {
		stateMark := mark
		if socketOutbound && (ordinal == 0 || ordinal == 3) {
			stateMark = 0
		}
		key := scopedXFRMKey{Kind: "state", Src: state.Src, Dst: state.Dst, SPI: state.SPI, Protocol: unix.IPPROTO_ESP, Mark: stateMark}
		if validateScopedXFRMKey(key) != nil {
			return nil, ErrXFRMSchema
		}
		info := nl.XfrmUsersaInfo{Saddr: scopedXFRMAddress(state.Src), Family: scopedXFRMFamily(state.Src), Reqid: state.ReqID,
			ReplayWindow: 32, Lft: scopedXFRMLifetime()}
		info.Id = nl.XfrmId{Daddr: scopedXFRMAddress(state.Dst), Spi: nl.Swap32(state.SPI), Proto: unix.IPPROTO_ESP}
		// ESP state selector pins endpoints, never an interface-index substitute.
		info.Sel = scopedXFRMSelector(key)
		info.Sel.Proto = 0
		// The maximum supported SA is 428 bytes. Reserve before copying any
		// keys so subsequent appends cannot abandon a key-bearing old buffer.
		wire := make([]byte, nl.SizeofXfrmUsersaInfo, 512)
		copy(wire, info.Serialize())
		auth := nl.XfrmAlgoAuth{AlgKeyLen: uint32(len(config.IntegrityKey) * 8), AlgTruncLen: 96, AlgKey: config.IntegrityKey}
		name := "hmac(sha1)"
		if imssec.IntegrityAlgorithm(config) == "hmac-md5-96" {
			name = "hmac(md5)"
		}
		copy(auth.AlgName[:], name)
		authWire := auth.Serialize()
		wire = scopedXFRMAttr(wire, nl.XFRMA_ALG_AUTH_TRUNC, authWire)
		clear(authWire)
		crypt := nl.XfrmAlgo{AlgKeyLen: uint32(len(config.EncryptionKey) * 8), AlgKey: config.EncryptionKey}
		switch imssec.EncryptionAlgorithm(config) {
		case "aes-cbc":
			name = "cbc(aes)"
		case "des-ede3-cbc":
			name = "cbc(des3_ede)"
		case "null":
			name = "ecb(cipher_null)"
		default:
			clear(wire)
			return nil, ErrXFRMSchema
		}
		copy(crypt.AlgName[:], name)
		cryptWire := crypt.Serialize()
		wire = scopedXFRMAttr(wire, nl.XFRMA_ALG_CRYPT, cryptWire)
		clear(cryptWire)
		wire = scopedXFRMAttr(wire, nl.XFRMA_MARK, scopedXFRMMark(stateMark))
		snapshot, err := scopedXFRMSnapshotOf("state", wire)
		if err != nil {
			clear(wire)
			return nil, err
		}
		out.Operations = append(out.Operations, scopedXFRMOperation{Key: key, Fingerprint: snapshot.MatchFingerprint, wireAdd: wire})
	}
	for _, policy := range plan.Policies {
		dir := uint8(0)
		if policy.Direction == "out" {
			dir = 1
		}
		key := scopedXFRMKey{Kind: "policy", Src: policy.Src.Addr(), Dst: policy.Dst.Addr(), Protocol: policy.Protocol,
			SrcPort: policy.SrcPort, DstPort: policy.DstPort, Direction: dir, Mark: mark}
		if validateScopedXFRMKey(key) != nil {
			return nil, ErrXFRMSchema
		}
		info := nl.XfrmUserpolicyInfo{Sel: scopedXFRMSelector(key), Lft: scopedXFRMLifetime(), Dir: dir, Priority: 1000}
		wire := append([]byte(nil), info.Serialize()...)
		tmpl := nl.XfrmUserTmpl{Family: scopedXFRMFamily(policy.TemplateSrc), Saddr: scopedXFRMAddress(policy.TemplateSrc),
			Reqid: policy.ReqID, Aalgos: 0xffffffff, Ealgos: 0xffffffff, Calgos: 0xffffffff}
		tmpl.XfrmId = nl.XfrmId{Daddr: scopedXFRMAddress(policy.TemplateDst), Spi: nl.Swap32(policy.SPI), Proto: unix.IPPROTO_ESP}
		wire = scopedXFRMAttr(wire, nl.XFRMA_TMPL, tmpl.Serialize())
		wire = scopedXFRMAttr(wire, nl.XFRMA_MARK, scopedXFRMMark(mark))
		snapshot, err := scopedXFRMSnapshotOf("policy", wire)
		if err != nil {
			clear(wire)
			return nil, err
		}
		out.Operations = append(out.Operations, scopedXFRMOperation{Key: key, Fingerprint: snapshot.MatchFingerprint, wireAdd: wire})
	}
	success = true
	return out, nil
}
