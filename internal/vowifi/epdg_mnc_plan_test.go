package vowifi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
)

// JSON keeps the RED test compilable on the original product. Before the new
// field is implemented, the unknown-length evidence is ignored and the real
// orchestrator incorrectly keeps the two-digit realm on the three-digit ePDG.
func uncertainMNCFixture(t *testing.T) SIMIdentity {
	t.Helper()
	var id SIMIdentity
	if err := json.Unmarshal([]byte(`{"ICCID":"8901000000000000001","IMSI":"999123123456789","IMEI":"867530912345678","HomeMCC":"999","HomeMNC":"12","HomeMNCUncertain":true,"DiscoveryPLMNs":["999123"]}`), &id); err != nil {
		t.Fatal(err)
	}
	return id
}

type mncPlanTunnel struct {
	env      *fakeEnvironment
	requests []TunnelRequest
	failure  error
}

func (p *mncPlanTunnel) Start(ctx context.Context, r TunnelRequest) (TunnelSession, error) {
	p.requests = append(p.requests, r)
	if p.failure != nil {
		return nil, p.failure
	}
	if r.Identity.HomeMNC != "123" {
		return nil, &EPDGAccessError{Cause: errors.New("synthetic two-digit endpoint unreachable")}
	}
	return fakeTunnelProvider{p.env}.Start(ctx, r)
}

type mncPlanIMS struct {
	env      *fakeEnvironment
	requests []IMSRequest
}

func (p *mncPlanIMS) Start(ctx context.Context, r IMSRequest) (IMSSession, error) {
	p.requests = append(p.requests, r)
	return fakeIMSProvider{p.env}.Start(ctx, r)
}

type mncPlanMemory struct {
	options    EPDGDiscoveryOptions
	identities []SIMIdentity
	candidates []EPDGCandidate
}

func (m *mncPlanMemory) LoadDiscovery(context.Context, SIMIdentity) (EPDGDiscoveryOptions, error) {
	return m.options, nil
}
func (m *mncPlanMemory) RememberRegistered(_ context.Context, id SIMIdentity, c EPDGCandidate) error {
	m.identities = append(m.identities, id)
	m.candidates = append(m.candidates, c)
	return nil
}

func TestMNCPlanOrchestratorKeepsTunnelIMSAndHistoryOnSelectedRealm(t *testing.T) {
	e := newFakeEnvironment()
	e.identity = uncertainMNCFixture(t)
	d := discoveryDependencies(e)
	p := &mncPlanTunnel{env: e}
	ims := &mncPlanIMS{env: e}
	history := &mncPlanMemory{}
	d.Tunnel = p
	d.IMS = ims
	d.Discovery = history
	o, err := New(d, Options{DeviceID: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Disable(context.Background())
	state, err := o.Enable(context.Background())
	if err != nil {
		t.Fatalf("legal three-digit fallback failed: %v", err)
	}
	if !state.IMSReady || state.HomeMCC != "999" || state.HomeMNC != "123" {
		t.Fatalf("selected state: %+v", state)
	}
	if len(ims.requests) != 1 || ims.requests[0].Identity.HomeMNC != "123" || len(history.identities) != 1 || history.identities[0].HomeMNC != "123" {
		t.Fatalf("realm split: IMS=%+v history=%+v", ims.requests, history.identities)
	}
	for _, r := range p.requests {
		if r.Identity.IMSI != e.identity.IMSI || r.Identity.ICCID != e.identity.ICCID {
			t.Fatal("MNC inference changed subscriber")
		}
	}
	if e.identity.HomeMNC != "12" {
		t.Fatal("attempt mutated the verified SIM snapshot")
	}
}

func TestMNCPlanSharedHomeCandidateContract(t *testing.T) {
	var c struct {
		Version int
		Cases   []struct {
			ID, IMSI, MNC      string
			Known              bool
			Equivalents, Homes []string
		}
	}
	b, err := os.ReadFile("../../../contracts/ims-home-mnc-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &c); err != nil || c.Version != 1 {
		t.Fatalf("contract: %v", err)
	}
	for _, v := range c.Cases {
		t.Run(v.ID, func(t *testing.T) {
			id := SIMIdentity{ICCID: "synthetic", IMSI: v.IMSI, HomeMCC: v.IMSI[:3], HomeMNC: v.MNC, HomeMNCUncertain: !v.Known, DiscoveryPLMNs: v.Equivalents}
			got, err := AuthenticationHomePLMNs(id)
			if err != nil || !reflect.DeepEqual(got, v.Homes) {
				t.Fatalf("homes=%v want=%v err=%v", got, v.Homes, err)
			}
		})
	}
}

func TestMNCPlanRejectsForeignOrMalformedUncertainHome(t *testing.T) {
	for _, change := range []func(*SIMIdentity){func(id *SIMIdentity) { id.HomeMCC = "204" }, func(id *SIMIdentity) { id.IMSI = "999x23123456789" }, func(id *SIMIdentity) { id.HomeMNC = "99" }, func(id *SIMIdentity) { id.IMSI = "99912" }} {
		id := uncertainMNCFixture(t)
		change(&id)
		if _, err := BuildEPDGAttempts(id, EPDGDiscoveryOptions{}); !errors.Is(err, ErrInvalidIdentity) {
			t.Fatalf("accepted invalid identity: %+v %v", id, err)
		}
	}
}

func TestMNCPlanManualStandardAndKnownAuthority(t *testing.T) {
	id := uncertainMNCFixture(t)
	host := standardEPDGHostname("999", "123")
	options := EPDGDiscoveryOptions{ManualOnly: true, PreferredAddresses: []string{host}}
	plans, err := BuildEPDGAttempts(id, options)
	if err != nil || len(plans) != 1 || plans[0].Identity.HomeMNC != "123" {
		t.Fatalf("manual inference: %v %v", plans, err)
	}
	id.HomeMNCUncertain = false
	plans, err = BuildEPDGAttempts(id, options)
	if err != nil || len(plans) != 1 || plans[0].Identity.HomeMNC != "12" {
		t.Fatalf("known MNC changed: %v %v", plans, err)
	}
	id.HomeMNCUncertain = true
	options.PreferredAddresses = []string{standardEPDGHostname("204", "04")}
	plans, err = BuildEPDGAttempts(id, options)
	if err != nil || len(plans) != 2 {
		t.Fatalf("foreign endpoint plans: %v %v", plans, err)
	}
	for _, p := range plans {
		if p.Identity.HomeMCC != "999" {
			t.Fatal("foreign ePDG replaced authentication home")
		}
	}
}

func TestMNCPlanLastSuccessIsAHostRealmPairAndManualPolicyStillWins(t *testing.T) {
	id := uncertainMNCFixture(t)
	options := EPDGDiscoveryOptions{LastSuccessful: &EPDGSelection{Host: "worked.example", HomeMCC: "999", HomeMNC: "123"}}
	plans, err := BuildEPDGAttempts(id, options)
	if err != nil || plans[0].Candidate.Host != "worked.example" || plans[0].Identity.HomeMNC != "123" {
		t.Fatalf("last pair lost: %v %v", plans, err)
	}
	options.ManualOnly = true
	options.PreferredAddresses = []string{"manual.example"}
	plans, err = BuildEPDGAttempts(id, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if p.Candidate.Host != "manual.example" {
			t.Fatal("removed history escaped manual-only")
		}
	}
	options.LastSuccessful.HomeMCC = "204"
	plans, err = BuildEPDGAttempts(id, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if p.Identity.HomeMCC != "999" {
			t.Fatal("history invented foreign home")
		}
	}
}

func TestMNCPlanBoundsPairsWithoutAliasingSubscriberHints(t *testing.T) {
	id := uncertainMNCFixture(t)
	options := EPDGDiscoveryOptions{}
	for i := 0; i < 16; i++ {
		options.PreferredAddresses = append(options.PreferredAddresses, fmt.Sprintf("manual-%d.example", i))
		options.AdditionalPLMNs = append(options.AdditionalPLMNs, fmt.Sprintf("204%02d", i))
	}
	plans, err := BuildEPDGAttempts(id, options)
	if err != nil || len(plans) != 64 {
		t.Fatalf("bound: %d %v", len(plans), err)
	}
	seen := map[string]bool{}
	for _, p := range plans {
		key := p.Candidate.Host + "/" + p.Identity.HomeMCC + p.Identity.HomeMNC
		if seen[key] || !AuthenticationHomeAllowed(id, p.Identity.HomeMCC, p.Identity.HomeMNC) {
			t.Fatal("duplicate or illegal pair")
		}
		seen[key] = true
	}
	plans[0].Identity.DiscoveryPLMNs[0] = "20404"
	if id.DiscoveryPLMNs[0] != "999123" || plans[1].Identity.DiscoveryPLMNs[0] != "999123" {
		t.Fatal("attempt hints alias the reader or another attempt")
	}
}

func TestMNCPlanAuthenticationOrCancellationNeverCyclesRealms(t *testing.T) {
	for _, failure := range []error{ErrEAPAuthenticationRejected, ErrResponderAUTHRequired, context.Canceled, errors.Join(&EPDGAccessError{Cause: errors.New("network")}, ErrResponderAUTHRequired)} {
		t.Run(failure.Error(), func(t *testing.T) {
			e := newFakeEnvironment()
			e.identity = uncertainMNCFixture(t)
			d := discoveryDependencies(e)
			p := &mncPlanTunnel{env: e, failure: failure}
			ims := &mncPlanIMS{env: e}
			memory := &mncPlanMemory{}
			d.Tunnel = p
			d.IMS = ims
			d.Discovery = memory
			o, err := New(d, Options{DeviceID: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			defer o.Disable(context.Background())
			if _, err = o.Enable(context.Background()); err == nil || len(p.requests) != 1 || len(ims.requests) != 0 || len(memory.identities) != 0 {
				t.Fatalf("authentication boundary crossed: %v %d", err, len(p.requests))
			}
		})
	}
}

func TestMNCPlanIMSFailureIsNotAnAccessRealmRetry(t *testing.T) {
	e := newFakeEnvironment()
	e.identity = uncertainMNCFixture(t)
	e.identity.DiscoveryPLMNs = nil // force a real two -> three access failover
	e.setFailure("ims.start", 1)
	d := discoveryDependencies(e)
	p := &mncPlanTunnel{env: e}
	memory := &mncPlanMemory{}
	d.Tunnel = p
	d.Discovery = memory
	o, err := New(d, Options{DeviceID: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Disable(context.Background())
	if _, err = o.Enable(context.Background()); err == nil || len(p.requests) != 2 || len(memory.identities) != 0 {
		t.Fatalf("IMS failure cycled realms: %v %d", err, len(p.requests))
	}
}

type mncPlanProxy struct {
	env             *fakeEnvironment
	requests        []ProxyRequest
	failSecondRealm bool
}

func (p *mncPlanProxy) Resolve(ctx context.Context, r ProxyRequest) (ProxyRoute, error) {
	p.requests = append(p.requests, r)
	if p.failSecondRealm && r.HomeMNC == "123" {
		return ProxyRoute{}, errors.New("explicit proxy unavailable")
	}
	return fakeProxy{p.env}.Resolve(ctx, r)
}

func TestMNCPlanFreshCardAccessFailoverRefreshesProxyRealm(t *testing.T) {
	for _, proxyFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(proxyFailure), func(t *testing.T) {
			e := newFakeEnvironment()
			e.identity = uncertainMNCFixture(t)
			e.identity.DiscoveryPLMNs = nil
			d := discoveryDependencies(e)
			p := &mncPlanTunnel{env: e}
			ims := &mncPlanIMS{env: e}
			proxy := &mncPlanProxy{env: e, failSecondRealm: proxyFailure}
			d.Tunnel = p
			d.IMS = ims
			d.Proxy = proxy
			o, err := New(d, Options{DeviceID: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			defer o.Disable(context.Background())
			state, err := o.Enable(context.Background())
			if len(proxy.requests) != 2 || proxy.requests[0].HomeMNC != "12" || proxy.requests[1].HomeMNC != "123" {
				t.Fatalf("stale proxy identity: %+v", proxy.requests)
			}
			if proxyFailure {
				if err == nil || len(p.requests) != 1 || len(ims.requests) != 0 {
					t.Fatal("proxy failure downgraded into direct access")
				}
				return
			}
			if err != nil || len(p.requests) != 2 || p.requests[0].Identity.HomeMNC != "12" || p.requests[1].Identity.HomeMNC != "123" || !state.HomeMNCUncertain || state.HomeMNC != "123" {
				t.Fatalf("access failover lost realm: %+v %v", state, err)
			}
		})
	}
}

func TestEC20MNCPlanApplicationReaderOptInDoesNotForgeAuthority(t *testing.T) {
	for _, encoded := range []string{"FFFFFF", "99F921993921", "02F44015F566"} {
		t.Run(encoded, func(t *testing.T) {
			const imsi = "999123123456789"
			steps := identityTranscriptStepsWithoutEFAD(imsi)
			steps = append(steps, ec20TranscriptStep{command: "AT+CRSM=176,28589,0,0,4", lines: []string{`+CRSM: 144,0,"000000FF"`}}, ec20TranscriptStep{command: "AT+CRSM=176,28589,0,0,0", lines: []string{`+CRSM: 144,0,"000000FF"`}}, ec20TranscriptStep{command: "AT+CRSM=176,28633,0,0,0", lines: []string{`+CRSM: 144,0,"` + encoded + `"`}}, ec20TranscriptStep{command: "AT+CIMI", lines: []string{imsi}}, ec20TranscriptStep{command: "AT+CCID", lines: []string{"+CCID: 8944101234567890123"}})
			x := &ec20Transcript{t: t, steps: steps}
			a, _ := NewEC20Adapter(x, EC20AdapterOptions{AllowUnresolvedMNCLength: true})
			id, err := a.ReadIdentity(context.Background(), "fixture")
			if err != nil || !id.HomeMNCUncertain || id.HomeMCC != "999" {
				t.Fatalf("uncertainty lost: %+v %v", id, err)
			}
			homes, err := AuthenticationHomePLMNs(id)
			if err != nil || len(homes) != 2 {
				t.Fatalf("legal prefixes lost: %v %v", homes, err)
			}
			x.assertDone()
		})
	}
}
