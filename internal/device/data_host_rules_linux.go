//go:build linux

package device

import (
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// netlink v1.3.1 RuleList does not copy the wire action into Rule.Type and
// ignores unknown selectors. Read the original dump so a changed blackhole,
// goto, interface, source, UID or future selector cannot match our receipt.
func dataReceiptRules(name string, family int) ([]netlink.Rule, error) {
	af := dataReceiptFamily(family)
	request := nl.NewNetlinkRequest(unix.RTM_GETRULE, unix.NLM_F_DUMP|unix.NLM_F_REQUEST)
	request.AddData(nl.NewIfInfomsg(af))
	messages, err := request.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWRULE)
	if err != nil {
		return nil, err
	}
	mark, table, priority := exportProxyRouteIdentity(name)
	var selected []netlink.Rule
	endian := nl.NativeEndian()
	for _, raw := range messages {
		if len(raw) < unix.SizeofRtMsg {
			return nil, ErrDataCleanupPending
		}
		header := nl.DeserializeRtMsg(raw)
		attributes, err := nl.ParseRouteAttr(raw[header.Len():])
		if err != nil {
			return nil, err
		}
		values := map[uint16][]byte{}
		duplicate := false
		for _, attr := range attributes {
			if _, exists := values[attr.Attr.Type]; exists {
				duplicate = true
			}
			values[attr.Attr.Type] = attr.Value
		}
		rule := netlink.NewRule()
		rule.Priority, rule.Family, rule.Table, rule.Type = 0, int(header.Family), int(header.Table), header.Type
		for kind, target := range map[uint16]*int{unix.RTA_TABLE: &rule.Table, nl.FRA_PRIORITY: &rule.Priority} {
			if value, present := values[kind]; present {
				if len(value) != 4 {
					return nil, ErrDataCleanupPending
				}
				*target = int(endian.Uint32(value))
			}
		}
		if value, present := values[nl.FRA_FWMARK]; present {
			if len(value) != 4 {
				return nil, ErrDataCleanupPending
			}
			rule.Mark = endian.Uint32(value)
		}
		if rule.Mark != mark && rule.Table != table && rule.Priority != priority {
			continue
		}
		// Extended tables use RT_TABLE_COMPAT in the legacy eight-bit field.
		// Their full table number has already been read from FRA_TABLE above.
		compatibleTable := header.Table == unix.RT_TABLE_COMPAT && rule.Table > 255 && len(values[unix.RTA_TABLE]) == 4
		if duplicate || rule.Family != af || header.Src_len != 0 || header.Dst_len != 0 || header.Tos != 0 || header.Protocol != 0 || header.Scope != 0 || header.Flags != 0 || header.Type != 1 || header.Table != 0 && int(header.Table) != rule.Table && !compatibleTable {
			return nil, ErrDataCleanupPending
		}
		for kind, value := range values {
			switch kind {
			case unix.RTA_TABLE, nl.FRA_PRIORITY, nl.FRA_FWMARK:
				// Checked while selecting this exact scope above.
			case nl.FRA_FWMASK:
				if len(value) != 4 {
					return nil, ErrDataCleanupPending
				}
				mask := endian.Uint32(value)
				rule.Mask = &mask
			case nl.FRA_PROTOCOL:
				if len(value) != 1 {
					return nil, ErrDataCleanupPending
				}
				rule.Protocol = value[0]
			case nl.FRA_SUPPRESS_IFGROUP, nl.FRA_SUPPRESS_PREFIXLEN:
				if len(value) != 4 || endian.Uint32(value) != ^uint32(0) {
					return nil, ErrDataCleanupPending
				}
			case nl.FRA_PAD:
				if len(value) != 0 {
					return nil, ErrDataCleanupPending
				}
			default:
				return nil, ErrDataCleanupPending
			}
		}
		selected = append(selected, *rule)
	}
	return selected, nil
}
