package ike

import (
	"context"
	"errors"
	"net"
	"reflect"
	"syscall"
	"testing"

	"vocat/internal/vowifi"
)

func TestIMSGatewayProductionTransportExcludesBeforeDirectOrProxyDial(t *testing.T) {
	for _, mode := range []vowifi.ProxyMode{vowifi.ProxyModeDirect, vowifi.ProxyModeSOCKS5} {
		for _, tc := range []struct{ host, excluded string }{{"203.0.113.1", "::ffff:203.0.113.1"}, {"2001:db8::1", "2001:0db8:0:0:0:0:0:1"}} {
			t.Run(string(mode)+"/"+tc.host, func(t *testing.T) {
				called := false
				dialer := &net.Dialer{Control: func(string, string, syscall.RawConn) error { called = true; return errors.New("unexpected socket") }}
				_, err := newDatagramTransport(context.Background(), transportConfig{Dialer: dialer, ExcludedAddresses: []string{tc.excluded}}, vowifi.ProxyRoute{Mode: mode}, tc.host)
				if !errors.Is(err, vowifi.ErrEPDGAddressesExhausted) || called {
					t.Fatalf("excluded peer reached network: err=%v dial=%v", err, called)
				}
			})
		}
	}
}

func TestIMSGatewayProductionProviderPassesDetachedExclusions(t *testing.T) {
	p, err := NewProvider(Config{Installer: unusedInstaller{}})
	if err != nil {
		t.Fatal(err)
	}
	excluded := []string{"203.0.113.1"}
	called := false
	p.transportFactory = func(_ context.Context, c transportConfig, _ vowifi.ProxyRoute, _ string) (datagramTransport, error) {
		called = true
		if !reflect.DeepEqual(c.ExcludedAddresses, excluded) {
			t.Error("provider dropped exclusions")
		}
		c.ExcludedAddresses[0] = "203.0.113.250"
		return nil, context.Canceled
	}
	aka := &testAKAProvider{}
	_, err = p.Start(context.Background(), vowifi.TunnelRequest{Identity: vowifi.SIMIdentity{ICCID: "8944100000000000000", IMSI: "234150123456789", HomeMCC: "234", HomeMNC: "15"}, AKA: aka, EPDG: "one.example", ExcludedEPDGAddresses: excluded})
	if !errors.Is(err, context.Canceled) || !called || excluded[0] != "203.0.113.1" || aka.calls != 0 {
		t.Fatalf("exclusions/AKA changed: %v %v calls=%d", excluded, err, aka.calls)
	}
}
