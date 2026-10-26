package vowifi

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEPDGCandidatesPreserveAuthenticationIdentityAndHistory(t *testing.T) {
	id := SIMIdentity{ICCID: "8944100000000000001", IMSI: "204041234567890", HomeMCC: "204", HomeMNC: "04", DiscoveryPLMNs: []string{"51566", "23415"}}
	before := id
	options := EPDGDiscoveryOptions{PreferredAddresses: []string{"MANUAL.example."}, AdditionalPLMNs: []string{"262-02"}, LastSuccessful: &EPDGSelection{Host: "success.example", HomeMCC: "204", HomeMNC: "04"}}
	candidates, err := BuildEPDGCandidates(id, options)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"success.example", "manual.example", "epdg.epc.mnc004.mcc204.pub.3gppnetwork.org", "epdg.epc.mnc066.mcc515.pub.3gppnetwork.org", "epdg.epc.mnc015.mcc234.pub.3gppnetwork.org", "epdg.epc.mnc002.mcc262.pub.3gppnetwork.org"}
	var got []string
	for _, c := range candidates {
		got = append(got, c.Host)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates %v", got)
	}
	if !reflect.DeepEqual(id, before) {
		t.Fatal("discovery rewrote SIM authentication")
	}
	options.ManualOnly = true
	candidates, err = BuildEPDGCandidates(id, options)
	if err != nil || len(candidates) != 1 || candidates[0].Host != "manual.example" {
		t.Fatalf("removed history escaped manual-only: %v %v", candidates, err)
	}
	options.LastSuccessful.Host = "manual.example"
	candidates, _ = BuildEPDGCandidates(id, options)
	if candidates[0].Source != "last_registered" {
		t.Fatal("allowed history not prioritized")
	}
	options.LastSuccessful.HomeMNC = "99"
	candidates, _ = BuildEPDGCandidates(id, options)
	if candidates[0].Source != "manual" {
		t.Fatal("different authentication home reused history")
	}
}
func TestEPDGConfigurationValidation(t *testing.T) {
	for _, host := range []string{"https://gateway.example", "host:500", "user@host", "host/path", "127.0.0.1", "0.0.0.0", "::", "fe80::1", "ff02::1", "255.255.255.255", "240.0.0.1", "192.0.2.999", "a..test", "a.-bad.test"} {
		t.Run(host, func(t *testing.T) {
			if _, err := NormalizeEPDGHost(host); err == nil {
				t.Fatal("invalid endpoint accepted")
			}
		})
	}
	for _, host := range []string{"EPDG.EXAMPLE.", "203.0.113.7", "2001:db8::1", "10.0.0.7"} {
		if _, err := NormalizeEPDGHost(host); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range []EPDGDiscoveryOptions{{ManualOnly: true}, {PreferredAddresses: make([]string, 17)}, {AdditionalPLMNs: []string{"5-15-66"}}, {AdditionalPLMNs: []string{"515"}}} {
		if _, err := o.Normalize(); err == nil {
			t.Fatal("bad options accepted")
		}
	}
}
func TestIssuerCountryOnlyAddsDiscoveryHint(t *testing.T) {
	id := SIMIdentity{ICCID: "8944100000000000001", IMSI: "204041234567890", HomeMCC: "204", HomeMNC: "04"}
	got := applyAssignedCarrierRoute(id)
	if got.HomeMCC != id.HomeMCC || got.HomeMNC != id.HomeMNC || got.IMSI != id.IMSI {
		t.Fatal("ICCID replaced authentication identity")
	}
	if len(got.DiscoveryPLMNs) == 0 {
		t.Fatal("issuer discovery hint lost")
	}
}

type fallbackTunnel struct {
	environment *fakeEnvironment
	requests    []TunnelRequest
	fail        error
}

func (f *fallbackTunnel) Start(ctx context.Context, request TunnelRequest) (TunnelSession, error) {
	f.requests = append(f.requests, request)
	if len(f.requests) == 1 {
		return nil, f.fail
	}
	return fakeTunnelProvider{f.environment}.Start(ctx, request)
}

type discoveryMemory struct {
	options    EPDGDiscoveryOptions
	remembered []EPDGCandidate
}

func (d *discoveryMemory) LoadDiscovery(context.Context, SIMIdentity) (EPDGDiscoveryOptions, error) {
	return d.options, nil
}
func (d *discoveryMemory) RememberRegistered(_ context.Context, _ SIMIdentity, c EPDGCandidate) error {
	d.remembered = append(d.remembered, c)
	return nil
}
func TestOrchestratorEPDGFallbackAndRegisteredHistory(t *testing.T) {
	for _, item := range []struct {
		name    string
		failure error
		tries   int
		success bool
	}{{"network", &EPDGAccessError{Cause: errors.New("DNS failed")}, 2, true}, {"AKA", ErrEAPAuthenticationRejected, 1, false}, {"AUTH", ErrResponderAUTHRequired, 1, false}, {"cancel", context.Canceled, 1, false}} {
		t.Run(item.name, func(t *testing.T) {
			env := newFakeEnvironment()
			deps := discoveryDependencies(env)
			tunnel := &fallbackTunnel{environment: env, fail: item.failure}
			deps.Tunnel = tunnel
			memory := &discoveryMemory{options: EPDGDiscoveryOptions{PreferredAddresses: []string{"first.example", "second.example"}, ManualOnly: true}}
			deps.Discovery = memory
			o, err := New(deps, Options{DeviceID: "test", CleanupTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer o.Disable(context.Background())
			state, err := o.Enable(context.Background())
			if (err == nil) != item.success || len(tunnel.requests) != item.tries {
				t.Fatalf("result %v attempts=%d", err, len(tunnel.requests))
			}
			if len(memory.remembered) != map[bool]int{true: 1, false: 0}[item.success] {
				t.Fatal("remembered without IMS registration")
			}
			for _, request := range tunnel.requests {
				if request.Identity.IMSI != env.identity.IMSI || request.Identity.HomeMNC != env.identity.HomeMNC {
					t.Fatal("candidate changed authentication")
				}
			}
			if item.success && (state.EPDG != "second.example" || state.EPDGAttempt != 2) {
				t.Fatalf("wrong selected endpoint %+v", state)
			}
		})
	}
}
func TestOrchestratorDoesNotRememberTunnelWithoutIMS(t *testing.T) {
	env := newFakeEnvironment()
	env.setFailure("ims.start", 1)
	deps := discoveryDependencies(env)
	memory := &discoveryMemory{}
	deps.Discovery = memory
	o, err := New(deps, Options{DeviceID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Disable(context.Background())
	_, err = o.Enable(context.Background())
	if err == nil || len(memory.remembered) != 0 {
		t.Fatal("failed IMS remembered as successful ePDG")
	}
}
func TestEPDGTypedFallbackCannotHideAuthenticationFailure(t *testing.T) {
	if EPDGFallbackAllowed(errors.Join(&EPDGAccessError{Cause: errors.New("network")}, ErrEAPAuthenticationRejected)) {
		t.Fatal("joined authentication failure was downgraded")
	}
	if strings.Contains((&EPDGAccessError{Cause: errors.New("network")}).Error(), "IMSI") {
		t.Fatal("unexpected identity data")
	}
}
func discoveryDependencies(env *fakeEnvironment) Dependencies {
	return Dependencies{SIM: fakeSIM{env}, AKA: fakeAKA{env}, Radio: fakeRadio{env}, Proxy: fakeProxy{env}, Tunnel: fakeTunnelProvider{env}, IMS: fakeIMSProvider{env}, Phones: fakePhones{env}}
}
func TestDITOIdentityRewriteCannotMatchUnrelated20404Subscriptions(t *testing.T) {
	for _, id := range []SIMIdentity{{ICCID: "8944100000000000001", IMSI: "204041234567890", HomeMCC: "204", HomeMNC: "04"}, {ICCID: "8963662600000000001", IMSI: "204041234567890", HomeMCC: "204", HomeMNC: "04"}, {ICCID: "8944100000000000001", IMSI: "204047616000001", HomeMCC: "204", HomeMNC: "04"}} {
		profile := ResolveCarrierProfile(id)
		if profile.RouteMCC == "515" || profile.EffectiveSubscriberIMSI(id.IMSI) != id.IMSI {
			t.Fatal("DITO rewriting selected without all subscriber selectors")
		}
	}
}
