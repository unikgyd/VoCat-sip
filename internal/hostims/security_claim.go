package hostims

import (
	"encoding/json"
	"net/netip"
	"strings"
)

// These bounded records contain selectors and fingerprints, never AKA keys or
// netlink add payloads. Unknown ACKs remain pending, not inferred from a match.
type securityFlow struct {
	Local  netip.Addr `json:"l"`
	Remote netip.Addr `json:"r"`
}

type securityAgreementRecord struct {
	Local, Remote                                                netip.Addr
	Integrity, Encryption                                        string
	UEClientSPI, UEServerSPI, PCSCFClientSPI, PCSCFServerSPI     uint32
	UEClientPort, UEServerPort, PCSCFClientPort, PCSCFServerPort int
}

type securityOperation struct {
	Kind    string          `json:"k"` // tc or xfrm; remove- prefix only for pending
	Ordinal uint8           `json:"o,omitempty"`
	Key     json.RawMessage `json:"x,omitempty"`
	Shape   string          `json:"s,omitempty"`
	Hash    string          `json:"h,omitempty"`
}

type securityClaim struct {
	Token       string                   `json:"t"`
	Phase       string                   `json:"p"`
	Mark        uint32                   `json:"m"`
	ClientReqID uint32                   `json:"c"`
	ServerReqID uint32                   `json:"s"`
	Flows       []securityFlow           `json:"f"`
	Agreement   *securityAgreementRecord `json:"a,omitempty"`
	Operations  []securityOperation      `json:"o,omitempty"`
	Pending     *securityOperation       `json:"q,omitempty"`
}

func cloneSecurityClaim(value *securityClaim) *securityClaim {
	if value == nil {
		return nil
	}
	result := *value
	result.Flows = append([]securityFlow(nil), value.Flows...)
	result.Operations = make([]securityOperation, len(value.Operations))
	for index, op := range value.Operations {
		result.Operations[index] = op
		result.Operations[index].Key = append(json.RawMessage(nil), op.Key...)
	}
	if value.Agreement != nil {
		agreement := *value.Agreement
		result.Agreement = &agreement
	}
	if value.Pending != nil {
		pending := *value.Pending
		pending.Key = append(json.RawMessage(nil), pending.Key...)
		result.Pending = &pending
	}
	return &result
}

func validSecurityIDs(mark, client, server uint32) bool {
	return mark >= 0x60000000 && mark <= 0x6fffffff && client >= 0x80000000 && client <= 0xbfffffff &&
		server >= 0x80000000 && server <= 0xbfffffff && client != server
}

func validSecurityClaim(value *securityClaim) bool {
	if value == nil {
		return true
	}
	if !hashString(value.Token) || !validSecurityIDs(value.Mark, value.ClientReqID, value.ServerReqID) ||
		len(value.Flows) == 0 || len(value.Flows) > 8 || len(value.Operations) > 21 {
		return false
	}
	switch value.Phase {
	case "reserving", "listening", "installing", "active", "cleaning", "uncertain":
	default:
		return false
	}
	seen := make(map[string]bool)
	var previous securityFlow
	for index, flow := range value.Flows {
		key := flow.Local.String() + "|" + flow.Remote.String()
		if !usable(flow.Local) || !usable(flow.Remote) || flow.Local == flow.Remote || flow.Local.Is4() != flow.Remote.Is4() ||
			seen[key] || index > 0 && (previous.Local.Compare(flow.Local) > 0 || previous.Local == flow.Local && previous.Remote.Compare(flow.Remote) >= 0) {
			return false
		}
		seen[key], previous = true, flow
	}
	if value.Agreement != nil && !validSecurityAgreement(*value.Agreement, value.Flows) {
		return false
	}
	validOp := func(op securityOperation, pending bool) bool {
		if op.Hash != "" && !hashString(op.Hash) || !pending && !hashString(op.Hash) {
			return false
		}
		switch strings.TrimPrefix(op.Kind, "remove-") {
		case "tc":
			return int(op.Ordinal) <= len(value.Flows) && len(op.Key) == 0 && op.Shape == ""
		case "xfrm":
			// The Linux driver additionally decodes the exact scalar schema and
			// verifies it against the agreement before any read/delete. Keeping
			// this portable record opaque avoids Linux-only types on Windows.
			return value.Agreement != nil && op.Ordinal < 12 && len(op.Key) > 1 && len(op.Key) <= 1024 &&
				op.Key[0] == '{' && json.Valid(op.Key) && hashString(op.Shape)
		default:
			return false
		}
	}
	seen = make(map[string]bool)
	for _, op := range value.Operations {
		key := op.Kind + ":" + string([]byte{op.Ordinal})
		if strings.HasPrefix(op.Kind, "remove-") || !validOp(op, false) || seen[key] {
			return false
		}
		seen[key] = true
	}
	if value.Pending == nil {
		return true
	}
	op := *value.Pending
	if !validOp(op, true) {
		return false
	}
	if !strings.HasPrefix(op.Kind, "remove-") {
		return op.Hash == "" && !seen[op.Kind+":"+string([]byte{op.Ordinal})]
	}
	op.Kind = strings.TrimPrefix(op.Kind, "remove-")
	for _, previous := range value.Operations {
		if previous.Kind == op.Kind && previous.Ordinal == op.Ordinal && previous.Shape == op.Shape &&
			previous.Hash == op.Hash && string(previous.Key) == string(op.Key) {
			return true
		}
	}
	return false
}

func validSecurityAgreement(value securityAgreementRecord, flows []securityFlow) bool {
	matched := false
	for _, flow := range flows {
		matched = matched || value.Local == flow.Local && value.Remote == flow.Remote
	}
	if !matched || value.Integrity != "hmac-sha-1-96" && value.Integrity != "hmac-md5-96" ||
		value.Encryption != "null" && value.Encryption != "aes-cbc" && value.Encryption != "des-ede3-cbc" {
		return false
	}
	spis := make(map[uint32]bool)
	for _, spi := range []uint32{value.UEClientSPI, value.UEServerSPI, value.PCSCFClientSPI, value.PCSCFServerSPI} {
		if spi < 256 || spis[spi] {
			return false
		}
		spis[spi] = true
	}
	for _, port := range []int{value.UEClientPort, value.UEServerPort, value.PCSCFClientPort, value.PCSCFServerPort} {
		if port <= 1024 || port > 65535 || port == 5060 || port == 5061 {
			return false
		}
	}
	return value.UEClientPort != value.UEServerPort && value.PCSCFClientPort != value.PCSCFServerPort
}
