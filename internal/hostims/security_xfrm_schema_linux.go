//go:build linux

package hostims

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// Fingerprint includes all supported static fields, including auto-assigned
// policy Index. MatchFingerprint excludes only that index for ADD-shape checks.
// No raw key material or raw kernel reply is exposed in a snapshot.
type scopedXFRMSnapshot struct {
	Fingerprint, MatchFingerprint           string
	IntegrityAlgorithm, EncryptionAlgorithm string
	PolicyIndex                             uint32
	mark, mask                              uint32
	reqids                                  []uint32
	state                                   *nl.XfrmUsersaInfo
	policy                                  *nl.XfrmUserpolicyInfo
}

type scopedXFRMAttribute struct {
	kind  uint16
	value []byte
}

func scopedXFRMAttributes(raw []byte) ([]scopedXFRMAttribute, error) {
	if len(raw) > 32768 {
		return nil, ErrXFRMSchema
	}
	var result []scopedXFRMAttribute
	seen := map[uint16]bool{}
	for len(raw) != 0 {
		if len(raw) < 4 {
			return nil, ErrXFRMSchema
		}
		length, kind := int(nl.NativeEndian().Uint16(raw[:2])), nl.NativeEndian().Uint16(raw[2:4])
		aligned := (length + 3) &^ 3
		if length < 4 || aligned > len(raw) || kind&0xc000 != 0 || !scopedXFRMZero(raw[length:aligned]) {
			return nil, ErrXFRMSchema
		}
		value := raw[4:length]
		if kind == nl.XFRMA_PAD {
			if !scopedXFRMZero(value) {
				return nil, ErrXFRMSchema
			}
		} else {
			if seen[kind] {
				return nil, ErrXFRMSchema
			}
			seen[kind] = true
			result = append(result, scopedXFRMAttribute{kind, value})
		}
		raw = raw[aligned:]
	}
	return result, nil
}

func scopedXFRMZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}
func scopedXFRMHash(kind string, header []byte, attrs []scopedXFRMAttribute) string {
	h := sha256.New()
	_, _ = h.Write([]byte("vocat-xfrm-static-v1/" + kind))
	_, _ = h.Write(header)
	sort.Slice(attrs, func(i, j int) bool { return attrs[i].kind < attrs[j].kind })
	for _, attr := range attrs {
		_, _ = h.Write(nl.NewRtAttr(int(attr.kind), attr.value).Serialize())
	}
	return hex.EncodeToString(h.Sum(nil))
}

func scopedXFRMValidateSelector(sel nl.XfrmSelector) bool {
	if sel.Family == 0 {
		return sel == (nl.XfrmSelector{})
	}
	if sel.Family != unix.AF_INET && sel.Family != unix.AF_INET6 || !scopedXFRMZero(sel.Pad[:]) || sel.Ifindex < 0 {
		return false
	}
	bits := uint8(128)
	if sel.Family == unix.AF_INET {
		bits = 32
		if !scopedXFRMZero(sel.Saddr[4:]) || !scopedXFRMZero(sel.Daddr[4:]) {
			return false
		}
	}
	return sel.PrefixlenS <= bits && sel.PrefixlenD <= bits
}

// Algorithm values are replaced by name/length/truncation plus SHA256(key).
// Compatibility ALG_AUTH duplicates must exactly agree with AUTH_TRUNC.
func scopedXFRMAlgorithm(attr scopedXFRMAttribute) ([]byte, error) {
	base := 68
	if attr.kind == nl.XFRMA_ALG_AUTH_TRUNC {
		base = 72
	}
	if len(attr.value) < base {
		return nil, ErrXFRMSchema
	}
	nameEnd := bytes.IndexByte(attr.value[:64], 0)
	if nameEnd <= 0 || !scopedXFRMZero(attr.value[nameEnd:64]) {
		return nil, ErrXFRMSchema
	}
	name := string(attr.value[:nameEnd])
	bits := nl.NativeEndian().Uint32(attr.value[64:68])
	if bits%8 != 0 || uint64(base)+uint64(bits)/8 != uint64(len(attr.value)) {
		return nil, ErrXFRMSchema
	}
	if attr.kind == nl.XFRMA_ALG_CRYPT {
		valid := name == "cbc(aes)" && bits == 128 || name == "cbc(des3_ede)" && bits == 192 || name == "ecb(cipher_null)" && bits == 0
		if !valid {
			return nil, ErrXFRMSchema
		}
	} else {
		if !(name == "hmac(sha1)" && bits == 160 || name == "hmac(md5)" && bits == 128) {
			return nil, ErrXFRMSchema
		}
		if base == 72 && nl.NativeEndian().Uint32(attr.value[68:72]) != 96 {
			return nil, ErrXFRMSchema
		}
	}
	result := append([]byte(nil), attr.value[:68]...)
	if base == 72 {
		result = append(result, attr.value[68:72]...)
	}
	hash := sha256.Sum256(attr.value[base:])
	return append(result, hash[:]...), nil
}

func scopedXFRMSnapshotOf(kind string, raw []byte) (scopedXFRMSnapshot, error) {
	var result scopedXFRMSnapshot
	headerSize := nl.SizeofXfrmUsersaInfo
	if kind == "policy" {
		headerSize = nl.SizeofXfrmUserpolicyInfo
	} else if kind != "state" {
		return result, ErrXFRMSchema
	}
	if len(raw) < headerSize || len(raw) > 32768 {
		return result, ErrXFRMSchema
	}
	header := append([]byte(nil), raw[:headerSize]...)
	if kind == "state" {
		info := *nl.DeserializeXfrmUsersaInfo(header)
		if (info.Family != unix.AF_INET && info.Family != unix.AF_INET6) || info.Id.Proto != unix.IPPROTO_ESP || info.Mode != 0 ||
			info.Flags&nl.XFRM_STATE_ESN != 0 || !scopedXFRMValidateSelector(info.Sel) ||
			!scopedXFRMZero(info.Pad[:]) || !scopedXFRMZero(info.Id.Pad[:]) {
			return result, ErrXFRMSchema
		}
		if info.Family == unix.AF_INET && (!scopedXFRMZero(info.Saddr[4:]) || !scopedXFRMZero(info.Id.Daddr[4:])) {
			return result, ErrXFRMSchema
		}
		info.Curlft, info.Stats, info.Seq = nl.XfrmLifetimeCur{}, nl.XfrmStats{}, 0
		copy(header, info.Serialize())
		result.state = &info
		result.reqids = []uint32{info.Reqid}
	} else {
		info := *nl.DeserializeXfrmUserpolicyInfo(header)
		if !scopedXFRMValidateSelector(info.Sel) || info.Dir > 4 || info.Action > 1 || info.Share > 3 || info.Flags&^uint8(3) != 0 || !scopedXFRMZero(info.Pad[:]) {
			return result, ErrXFRMSchema
		}
		info.Curlft = nl.XfrmLifetimeCur{}
		copy(header, info.Serialize())
		result.policy = &info
		result.PolicyIndex = info.Index
	}
	attrs, err := scopedXFRMAttributes(raw[headerSize:])
	if err != nil {
		return result, err
	}
	var canonical []scopedXFRMAttribute
	var compatAuth, auth []byte
	seenCrypt, seenTemplates := false, false
	for _, attr := range attrs {
		value := attr.value
		switch attr.kind {
		case nl.XFRMA_MARK:
			if len(value) != 8 {
				return result, ErrXFRMSchema
			}
			result.mark, result.mask = nl.NativeEndian().Uint32(value), nl.NativeEndian().Uint32(value[4:])
			if result.mark & ^result.mask != 0 {
				return result, ErrXFRMSchema
			}
			canonical = append(canonical, attr)
		case nl.XFRMA_ALG_AUTH, nl.XFRMA_ALG_AUTH_TRUNC, nl.XFRMA_ALG_CRYPT:
			if kind != "state" {
				return result, ErrXFRMSchema
			}
			normalized, err := scopedXFRMAlgorithm(attr)
			if err != nil {
				return result, err
			}
			if attr.kind == nl.XFRMA_ALG_AUTH {
				compatAuth = normalized
				continue
			}
			if attr.kind == nl.XFRMA_ALG_AUTH_TRUNC {
				auth = normalized
				result.IntegrityAlgorithm = "hmac-sha-1-96"
				if bytes.Equal(normalized[:9], []byte("hmac(md5)")) {
					result.IntegrityAlgorithm = "hmac-md5-96"
				}
			} else {
				seenCrypt = true
				switch string(normalized[:bytes.IndexByte(normalized[:64], 0)]) {
				case "cbc(aes)":
					result.EncryptionAlgorithm = "aes-cbc"
				case "cbc(des3_ede)":
					result.EncryptionAlgorithm = "des-ede3-cbc"
				case "ecb(cipher_null)":
					result.EncryptionAlgorithm = "null"
				default:
					return result, ErrXFRMSchema
				}
			}
			canonical = append(canonical, scopedXFRMAttribute{attr.kind, normalized})
		case nl.XFRMA_TMPL:
			if kind != "policy" || len(value) == 0 || len(value)%nl.SizeofXfrmUserTmpl != 0 || len(value) > 4*nl.SizeofXfrmUserTmpl {
				return result, ErrXFRMSchema
			}
			seenTemplates = true
			for pos := 0; pos < len(value); pos += nl.SizeofXfrmUserTmpl {
				tmpl := nl.DeserializeXfrmUserTmpl(value[pos:])
				if (tmpl.Family != unix.AF_INET && tmpl.Family != unix.AF_INET6) || tmpl.XfrmId.Proto != unix.IPPROTO_ESP ||
					tmpl.Mode != 0 || tmpl.Share > 3 || tmpl.Optional > 1 || !scopedXFRMZero(tmpl.Pad1[:]) || tmpl.Pad2 != 0 || !scopedXFRMZero(tmpl.XfrmId.Pad[:]) {
					return result, ErrXFRMSchema
				}
				if tmpl.Family == unix.AF_INET && (!scopedXFRMZero(tmpl.Saddr[4:]) || !scopedXFRMZero(tmpl.XfrmId.Daddr[4:])) {
					return result, ErrXFRMSchema
				}
				result.reqids = append(result.reqids, tmpl.Reqid)
			}
			canonical = append(canonical, attr)
		case nl.XFRMA_REPLAY_VAL, nl.XFRMA_LTIME_VAL, nl.XFRMA_LASTUSED:
			expected := map[uint16]int{nl.XFRMA_REPLAY_VAL: 12, nl.XFRMA_LTIME_VAL: 32, nl.XFRMA_LASTUSED: 8}[attr.kind]
			if kind != "state" || len(value) != expected {
				return result, ErrXFRMSchema
			}
			// Explicitly dynamic replay/counter/lifetime observations, not owner fields.
		case nl.XFRMA_IF_ID, nl.XFRMA_SA_EXTRA_FLAGS, nl.XFRMA_SET_MARK, nl.XFRMA_SET_MARK_MASK, nl.XFRMA_MTIMER_THRESH, nl.XFRMA_SA_PCPU:
			if len(value) != 4 || (kind != "state" && attr.kind != nl.XFRMA_IF_ID) {
				return result, ErrXFRMSchema
			}
			// Known absent/default encodings are canonicalized; all nondefaults
			// remain static fingerprint material (including if_id and mark masks).
			val := nl.NativeEndian().Uint32(value)
			if attr.kind == nl.XFRMA_SA_EXTRA_FLAGS && val & ^uint32(3) != 0 {
				return result, ErrXFRMSchema
			}
			if val == 0 && attr.kind != nl.XFRMA_SET_MARK_MASK || attr.kind == nl.XFRMA_SA_PCPU && val == 0xffffffff {
				continue
			}
			canonical = append(canonical, attr)
		case nl.XFRMA_SA_DIR:
			if kind != "state" || len(value) != 1 || value[0] > 2 {
				return result, ErrXFRMSchema
			}
			if value[0] != 0 {
				canonical = append(canonical, attr)
			}
		case nl.XFRMA_POLICY_TYPE:
			if kind != "policy" || len(value) != 4 || !scopedXFRMZero(value) {
				return result, ErrXFRMSchema
			}
		default:
			// Security context, offload, ESN, compression, encapsulation, AEAD and
			// future attributes are unsupported, NEVER silently discarded.
			return result, ErrXFRMSchema
		}
	}
	if kind == "state" {
		if len(auth) == 0 || !seenCrypt {
			return result, ErrXFRMSchema
		}
		if compatAuth != nil && (!bytes.Equal(compatAuth[:68], auth[:68]) || !bytes.Equal(compatAuth[68:], auth[72:])) {
			return result, ErrXFRMSchema
		}
	} else if result.policy.Action == 0 && !seenTemplates {
		return result, ErrXFRMSchema
	}
	result.Fingerprint = scopedXFRMHash(kind, header, canonical)
	if result.policy != nil {
		info := *result.policy
		info.Index = 0
		copy(header, info.Serialize())
	}
	result.MatchFingerprint = scopedXFRMHash(kind, header, canonical)
	return result, nil
}

func (s scopedXFRMSnapshot) matchesKey(k scopedXFRMKey) bool {
	if s.mark != k.Mark || s.mask != 0xffffffff {
		return false
	}
	if k.Kind == "state" {
		return s.state != nil && s.state.Family == scopedXFRMFamily(k.Dst) && s.state.Saddr == scopedXFRMAddress(k.Src) &&
			s.state.Id.Daddr == scopedXFRMAddress(k.Dst) && s.state.Id.Proto == k.Protocol && s.state.Id.Spi == nl.Swap32(k.SPI)
	}
	return s.policy != nil && s.policy.Dir == k.Direction && s.policy.Sel == scopedXFRMSelector(k)
}
