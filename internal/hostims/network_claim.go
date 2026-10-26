package hostims

import (
	"net/netip"
	"strings"
)

// Compact private journal records. A pending or uncertain operation is never
// inferred successful from a matching kernel object after a process restart.
type networkOperation struct {
	Kind     string       `json:"k"`
	Local    netip.Prefix `json:"l,omitempty"`
	Remote   netip.Addr   `json:"r,omitempty"`
	Gateway  netip.Addr   `json:"g,omitempty"`
	MTU      uint32       `json:"m,omitempty"`
	Hash     string       `json:"h,omitempty"`
	Owned    bool         `json:"o,omitempty"`
	Media    bool         `json:"v,omitempty"`
	Previous uint32       `json:"d,omitempty"`
}
type networkClaim struct {
	Token         string             `json:"t"`
	Phase         string             `json:"p"`
	Namespace     string             `json:"n"`
	Interface     string             `json:"i"`
	Index         int                `json:"x"`
	Anchor        string             `json:"a"`
	Table         int                `json:"b"`
	Configuration string             `json:"c"`
	Operations    []networkOperation `json:"s"`
	Pending       *networkOperation  `json:"q,omitempty"`
	Security      *securityClaim     `json:"e,omitempty"`
}

func cloneNetworkClaim(record *networkClaim) *networkClaim {
	if record == nil {
		return nil
	}
	next := *record
	next.Operations = append([]networkOperation(nil), record.Operations...)
	next.Security = cloneSecurityClaim(record.Security)
	if record.Pending != nil {
		pending := *record.Pending
		next.Pending = &pending
	}
	return &next
}
func validNetworkClaim(record *networkClaim) bool {
	if record == nil {
		return true
	}
	if !hashString(record.Token) || !hashString(record.Namespace) || !hashString(record.Anchor) || !hashString(record.Configuration) || !validInterface(record.Interface) || record.Index < 1 || record.Table < 0x40000000 || record.Table > 0x4fffffff || len(record.Operations) > 23 || !validSecurityClaim(record.Security) {
		return false
	}
	switch record.Phase {
	case "preparing", "active", "cleaning", "uncertain":
	default:
		return false
	}
	validOp := func(op networkOperation) bool {
		kind := strings.TrimPrefix(op.Kind, "remove-")
		if kind != "accept_ra" && kind != "autoconf" && op.Previous != 0 {
			return false
		}
		if !op.Local.IsValid() || !usable(op.Local.Addr()) || op.Hash != "" && !hashString(op.Hash) || op.MTU != 0 && (op.MTU > 65535 || op.MTU < 68 || op.Local.Addr().Is6() && op.MTU < 1280) {
			return false
		}
		switch strings.TrimPrefix(op.Kind, "remove-") {
		case "accept_ra", "autoconf":
			max := uint32(1)
			if kind == "accept_ra" {
				max = 2
			}
			return op.Owned && op.Previous > 0 && op.Previous <= max && !op.Remote.IsValid() && !op.Gateway.IsValid() && !op.Media && op.MTU == 0
		case "link":
			return op.Owned && !op.Remote.IsValid() && !op.Gateway.IsValid() && !op.Media && op.MTU == 0
		case "address", "rule":
			return !op.Remote.IsValid() && !op.Gateway.IsValid() && !op.Media && op.MTU == 0
		case "route":
			return op.Owned && usable(op.Remote) && op.Remote != op.Local.Addr() && op.Remote.Is4() == op.Local.Addr().Is4() && (!op.Gateway.IsValid() || usable(op.Gateway) && op.Gateway != op.Local.Addr() && op.Gateway.Is4() == op.Remote.Is4())
		default:
			return false
		}
	}
	seen := make(map[string]bool)
	for _, op := range record.Operations {
		key := op.Kind + ":" + op.Local.String()
		if op.Kind == "route" {
			key = op.Kind + ":" + op.Remote.String()
		}
		if !validOp(op) || strings.HasPrefix(op.Kind, "remove-") || !op.Owned && (op.Kind != "address" || !hashString(op.Hash)) || seen[key] {
			return false
		}
		seen[key] = true
	}
	if record.Pending == nil {
		return true
	}
	op := *record.Pending
	if !validOp(op) || !op.Owned {
		return false
	}
	if strings.HasPrefix(op.Kind, "remove-") {
		op.Kind = strings.TrimPrefix(op.Kind, "remove-")
		if !hashString(op.Hash) {
			return false
		}
		for _, previous := range record.Operations {
			if previous == op {
				return true
			}
		}
		return false
	}
	return op.Hash == ""
}

func networkReady(record *networkClaim) bool {
	if record == nil {
		return true
	}
	if record.Phase != "active" || record.Pending != nil {
		return false
	}
	if security := record.Security; security != nil && (security.Pending != nil || security.Phase != "listening" && security.Phase != "active") {
		return false
	}
	for _, op := range record.Operations {
		if !hashString(op.Hash) {
			return false
		}
	}
	return true
}
