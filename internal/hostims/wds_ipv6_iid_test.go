package hostims

import (
	"net/netip"
	"testing"
)

func TestOwnedIPv6RuntimeKeepsTheOriginalHostIIDWithinTheSamePDN(t *testing.T) {
	address := BearerAddress{Address: netip.MustParsePrefix("2001:db8:12:34::10/64"), MTU: 1432}
	row := Runtime{APN: "ims", Family: 6, IPv6: netip.MustParsePrefix("2001:db8:12:34::99/64"), MTU: 1432}
	actual, err := normalizeBoundRuntime(row, address, "ims")
	if err != nil || actual.IPv6 != address.Address {
		t.Fatal("client-specific IID replaced or blocked the original host address", err)
	}
	for _, bad := range []Runtime{
		{APN: "ims", Family: 6, IPv6: netip.MustParsePrefix("2001:db8:12:35::10/64"), MTU: 1432},
		{APN: "internet", Family: 6, IPv6: row.IPv6, MTU: 1432},
		{APN: "ims", Family: 6, IPv6: netip.MustParsePrefix("2001:db8:12:34::99/128"), MTU: 1432},
		{APN: "ims", Family: 6, IPv6: row.IPv6, MTU: 1500},
	} {
		if _, err := normalizeBoundRuntime(bad, address, "ims"); err == nil {
			t.Fatal("foreign PDN or changed runtime was accepted")
		}
	}
}
