package imssec

import (
	"net/netip"
	"testing"
)

func TestProtectedSocketBindingStrictValidation(t *testing.T) {
	good := SocketBinding{Local: netip.MustParseAddrPort("192.0.2.1:21000"), Remote: netip.MustParseAddr("192.0.2.2"), Role: ClientSocket}
	for _, transport := range []string{"tcp4", "udp4"} {
		if err := good.Validate(transport); err != nil {
			t.Fatal(err)
		}
	}
	v6 := SocketBinding{Local: netip.MustParseAddrPort("[2001:db8::1]:21001"), Remote: netip.MustParseAddr("2001:db8::2"), Role: ServerSocket}
	for _, transport := range []string{"tcp6", "udp6"} {
		if err := v6.Validate(transport); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name, transport string
		mutate          func(*SocketBinding)
	}{
		{"wrong_transport_family", "udp6", func(*SocketBinding) {}},
		{"unspecified_transport", "tcp", func(*SocketBinding) {}},
		{"raw_protocol", "ip4", func(*SocketBinding) {}},
		{"remote_family", "udp4", func(b *SocketBinding) { b.Remote = v6.Remote }},
		{"remote_zero", "udp4", func(b *SocketBinding) { b.Remote = netip.Addr{} }},
		{"remote_multicast", "udp4", func(b *SocketBinding) { b.Remote = netip.MustParseAddr("224.0.0.1") }},
		{"local_wildcard", "udp4", func(b *SocketBinding) { b.Local = netip.MustParseAddrPort("0.0.0.0:21000") }},
		{"mapped_v4", "udp6", func(b *SocketBinding) { b.Local = netip.MustParseAddrPort("[::ffff:192.0.2.1]:21000") }},
		{"local_zone", "udp6", func(b *SocketBinding) { b.Local = netip.MustParseAddrPort("[2001:db8::1%eth0]:21000") }},
		{"low_port", "udp4", func(b *SocketBinding) { b.Local = netip.AddrPortFrom(b.Local.Addr(), 1024) }},
		{"sip_port", "udp4", func(b *SocketBinding) { b.Local = netip.AddrPortFrom(b.Local.Addr(), 5060) }},
		{"tls_port", "udp4", func(b *SocketBinding) { b.Local = netip.AddrPortFrom(b.Local.Addr(), 5061) }},
		{"unknown_role", "udp4", func(b *SocketBinding) { b.Role = 3 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding := good
			test.mutate(&binding)
			if binding.Validate(test.transport) != ErrSocketBinding {
				t.Fatal("unsafe protected socket binding accepted")
			}
		})
	}
}
