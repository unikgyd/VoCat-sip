package vowifi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"vocat/internal/modem"
)

type profileHistoryFixture struct {
	discoveryMemory
	candidates []SIMIdentity
}

func (p *profileHistoryFixture) IdentityCandidates(context.Context, SIMIdentity) ([]SIMIdentity, error) {
	return p.candidates, nil
}

func TestProfileIdentityPreviouslySuccessfulSameCardIdentityReachesTunnel(t *testing.T) {
	e := newFakeEnvironment()
	historical := e.identity
	historical.IMSI = "204040123456789"
	historical.HomeMCC = "204"
	historical.HomeMNC = "04"
	history := &profileHistoryFixture{candidates: []SIMIdentity{historical, e.identity}}
	d := discoveryDependencies(e)
	d.Discovery = history
	var requests []TunnelRequest
	d.Tunnel = gatewayTunnelFunc(func(ctx context.Context, r TunnelRequest) (TunnelSession, error) {
		requests = append(requests, r)
		return fakeTunnelProvider{e}.Start(ctx, r)
	})
	o, err := New(d, Options{DeviceID: "profile-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Disable(context.Background())
	state, err := o.Enable(context.Background())
	if err != nil || !state.IMSReady {
		t.Fatalf("profile setup failed: %v", err)
	}
	if len(requests) == 0 || requests[0].Identity.IMSI != historical.IMSI {
		t.Fatalf("same-card history was ignored; selected=%v", requests)
	}
	if state.IMSI != e.identity.IMSI {
		t.Fatal("authentication identity replaced the physical SMS/call source")
	}
}

type profileIdentityAT struct{ commands []string }

func (p *profileIdentityAT) ExecuteAT(_ context.Context, _ string, command string) (modem.Response, error) {
	p.commands = append(p.commands, command)
	lines := map[string][]string{"AT+CPIN?": {"+CPIN: READY"}, "AT+CIMI": {"234150123456789"}, "AT+CCID": {"+CCID: 8944101234567890123"}, "AT+CGSN": {"867530912345678"}, "AT+CRSM=176,28589,0,0,4": {`+CRSM: 144,0,"00000002"`}, "AT+CRSM=176,28633,0,0,0": {`+CRSM: 144,0,"FFFFFF"`}, "AT+CRSM=176,28423,0,0,9": {`+CRSM: 144,0,"082940401032547698"`}}
	if value, ok := lines[command]; ok {
		return modem.Response{Command: command, Lines: value, Final: "OK"}, nil
	}
	return modem.Response{}, errors.New("test: unexpected AT command")
}

func TestProfileIdentityLiveEFIMSIReadCanReachAlternateAuthenticationHome(t *testing.T) {
	var options EC20AdapterOptions
	if err := json.Unmarshal([]byte(`{"AllowUnresolvedMNCLength":true,"ReadProfileIdentities":true}`), &options); err != nil {
		t.Fatal(err)
	}
	executor := &profileIdentityAT{}
	adapter, err := NewEC20Adapter(executor, options)
	if err != nil {
		t.Fatal(err)
	}
	id, err := adapter.ReadIdentity(context.Background(), "profile-modem")
	if err != nil {
		t.Fatal(err)
	}
	e := newFakeEnvironment()
	e.identity = id
	d := discoveryDependencies(e)
	d.Tunnel = gatewayTunnelFunc(func(ctx context.Context, r TunnelRequest) (TunnelSession, error) {
		if r.Identity.IMSI != "204040123456789" {
			return nil, &EPDGAccessError{Cause: errors.New("test: reported roaming identity has no ePDG")}
		}
		return fakeTunnelProvider{e}.Start(ctx, r)
	})
	o, err := New(d, Options{DeviceID: "profile-modem"})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Disable(context.Background())
	state, err := o.Enable(context.Background())
	reads := 0
	for _, command := range executor.commands {
		if strings.HasPrefix(command, "AT+CRSM=176,28423,") {
			reads++
		}
	}
	if err != nil || !state.IMSReady || state.HomeMCC != "204" || reads != 2 {
		t.Fatalf("live EF_IMSI candidate was not read/bound/selected: EF reads=%d ready=%v home=%s err=%v", reads, state.IMSReady, state.HomeMCC, err)
	}
}
