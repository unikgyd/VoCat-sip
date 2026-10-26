package vowifi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"vocat/internal/modem"
)

func TestProfileIdentityEFEncodingIsNotGenericBCD(t *testing.T) {
	for _, c := range []struct{ name, raw, want string }{
		{"odd", "082940401032547698", "204040123456789"}, {"even", "0821404010325476F8", "20404012345678"}, {"padding", "082940401032547698FFFF", "204040123456789"},
		{"truncated", "092940401032547698", ""}, {"identity-type", "082840401032547698", ""}, {"interior-F", "0829404F1032547698", ""}, {"wrong-parity", "082140401032547698", ""}, {"trailing", "08294040103254769800", ""}, {"length-overflow", "FF2940401032547698", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw, _ := hex.DecodeString(c.raw)
			got, err := parseProfileIMSI(raw)
			if c.want == "" {
				if err == nil {
					t.Fatal("invalid EF_IMSI accepted")
				}
			} else if err != nil || got != c.want {
				t.Fatalf("got %q error=%v want %s", got, err, c.want)
			}
		})
	}
}

type profilePolicyAT struct {
	profileIdentityAT
	mode          string
	authenticated bool
	fileReads     int
}

func (p *profilePolicyAT) ExecuteAT(ctx context.Context, device, command string) (modem.Response, error) {
	if strings.HasPrefix(command, "AT+CRSM=176,28423,") {
		p.fileReads++
		switch p.mode {
		case "missing":
			return modem.Response{Lines: []string{`+CRSM: 106,130`}, Final: "OK"}, nil
		case "io":
			return modem.Response{}, io.EOF
		case "security":
			return modem.Response{Lines: []string{`+CRSM: 105,130`}, Final: "OK"}, nil
		case "changed-file":
			if p.fileReads > 1 {
				return modem.Response{Lines: []string{`+CRSM: 144,0,"082940401032547699"`}, Final: "OK"}, nil
			}
		}
		if p.authenticated && p.mode == "file-after-auth" {
			return modem.Response{Lines: []string{`+CRSM: 144,0,"082940401032547699"`}, Final: "OK"}, nil
		}
	}
	if p.authenticated && p.mode == "reported-after-auth" && command == "AT+CIMI" {
		return modem.Response{Lines: []string{"234150123456780"}, Final: "OK"}, nil
	}
	if p.authenticated && p.mode == "card-after-auth" && command == "AT+CCID" {
		return modem.Response{Lines: []string{"+CCID: 8944101234567890124"}, Final: "OK"}, nil
	}
	switch {
	case command == "AT+CUAD":
		return modem.Response{Lines: []string{`+CUAD: "61094F07A0000000871002"`}, Final: "OK"}, nil
	case strings.HasPrefix(command, "AT+CCHO="):
		return modem.Response{Lines: []string{"+CCHO: 1"}, Final: "OK"}, nil
	case strings.HasPrefix(command, "AT+CCHC="):
		return modem.Response{Final: "OK"}, nil
	case strings.HasPrefix(command, "AT+CGLA="):
		p.authenticated = true
		encoded := strings.ToUpper(hex.EncodeToString(successfulUSIMResponse()))
		return modem.Response{Lines: []string{"+CGLA: " + fmtLength(encoded) + ",\"" + encoded + "\""}, Final: "OK"}, nil
	}
	return p.profileIdentityAT.ExecuteAT(ctx, device, command)
}
func fmtLength(value string) string { // no raw APDU/vector ever enters a failure message
	return strconv.Itoa(len(value))
}

func TestProfileIdentityReadFencesAndOptionalAbsence(t *testing.T) {
	for _, mode := range []string{"missing", "io", "security", "changed-file"} {
		t.Run(mode, func(t *testing.T) {
			executor := &profilePolicyAT{mode: mode}
			adapter, _ := NewEC20Adapter(executor, EC20AdapterOptions{ReadProfileIdentities: true, AllowUnresolvedMNCLength: true})
			id, err := adapter.ReadIdentity(context.Background(), "profile")
			if mode == "missing" {
				if err != nil || len(CurrentProfileIdentities(id)) != 1 {
					t.Fatalf("explicit absence blocked live source: %v", err)
				}
			} else {
				if err == nil || len(adapter.bindings) != 0 {
					t.Fatalf("failed read published a SIM binding: %s %v", mode, err)
				}
			}
		})
	}
}

func TestProfileIdentityAKAUsesLiveCardAndRechecksBothIMSIReads(t *testing.T) {
	for _, mode := range []string{"ok", "reported-after-auth", "file-after-auth", "card-after-auth"} {
		t.Run(mode, func(t *testing.T) {
			executor := &profilePolicyAT{mode: mode}
			adapter, _ := NewEC20Adapter(executor, EC20AdapterOptions{ReadProfileIdentities: true, AllowUnresolvedMNCLength: true})
			live, err := adapter.ReadIdentity(context.Background(), "profile")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = adapter.CheckReady(context.Background(), live); err != nil {
				t.Fatal(err)
			}
			selected := CurrentProfileIdentities(live)[1]
			bound, err := ProfileAuthenticationIdentity(live, selected)
			if err != nil {
				t.Fatal(err)
			}
			physical, err := AKAIdentity(bound)
			if err != nil {
				t.Fatal(err)
			}
			result, err := adapter.Authenticate(context.Background(), physical, AKAChallenge{})
			if mode == "ok" {
				if err != nil || len(result.RES) == 0 {
					t.Fatalf("same-card alternate cannot use AKA: %v", err)
				}
			} else if err == nil || len(result.RES) != 0 || len(result.CK) != 0 || len(result.IK) != 0 {
				t.Fatalf("changed source released AKA material: %s err=%v", mode, err)
			}
		})
	}
}

func TestProfileIdentityChangedAuthenticationFieldsCannotReuseSource(t *testing.T) {
	live := newFakeEnvironment().identity
	selected := live
	selected.IMSI = "204040123456789"
	bound, err := ProfileAuthenticationIdentity(live, selected)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*SIMIdentity){func(id *SIMIdentity) { id.ICCID += "9" }, func(id *SIMIdentity) { id.IMSI = "204040123456788" }, func(id *SIMIdentity) { id.HomeMCC = "515"; id.HomeMNC = "66" }} {
		copy := bound
		change(&copy)
		if _, err := AKAIdentity(copy); err == nil {
			t.Fatal("mutated attempt reused the physical binding")
		}
	}
	physical, err := AKAIdentity(bound)
	if err != nil || physical.IMSI != live.IMSI {
		t.Fatalf("physical source mismatch: %v", err)
	}
	if encoded, _ := json.Marshal(bound); strings.Contains(string(encoded), "profileSource") || strings.Contains(string(encoded), live.IMSI) {
		t.Fatal("private source token escaped through JSON")
	}
}

func TestProfileIdentityHistoryCannotCrossCardsOrOverrideCurrentEFAD(t *testing.T) {
	e := newFakeEnvironment()
	history := &profileHistoryFixture{candidates: []SIMIdentity{{ICCID: "another-card", IMSI: "204040123456789", HomeMCC: "204", HomeMNC: "04"}}}
	d := discoveryDependencies(e)
	d.Discovery = history
	o, _ := New(d, Options{DeviceID: "profile"})
	defer o.Disable(context.Background())
	if _, err := o.Enable(context.Background()); !errors.Is(err, ErrProfileIdentityBinding) {
		t.Fatalf("foreign history allowed: %v", err)
	}
	got, err := ProfileIdentityFromHistory(e.identity, e.identity.IMSI)
	if err != nil || !reflect.DeepEqual(got, e.identity) {
		t.Fatal("history overrode the current known home")
	}
}

func TestProfileIdentityIMSFailureDoesNotTryAnotherIMSIWithTheSameHome(t *testing.T) {
	e := newFakeEnvironment()
	historical := e.identity
	historical.IMSI = "234150999999999"
	d := discoveryDependencies(e)
	d.Discovery = &profileHistoryFixture{candidates: []SIMIdentity{historical, e.identity}}
	starts := 0
	d.Tunnel = gatewayTunnelFunc(func(ctx context.Context, r TunnelRequest) (TunnelSession, error) {
		if len(r.ExcludedEPDGAddresses) > 0 {
			return nil, ErrEPDGAddressesExhausted
		}
		return &gatewayTestTunnel{fakeTunnelSession: &fakeTunnelSession{e}, ip: "203.0.113.7"}, nil
	})
	d.IMS = gatewayIMSFunc(func(context.Context, IMSRequest) (IMSSession, error) { starts++; return nil, gatewayTimeout() })
	o, _ := New(d, Options{DeviceID: "profile"})
	defer o.Disable(context.Background())
	if _, err := o.Enable(context.Background()); err == nil || starts != 1 {
		t.Fatalf("IMS silence cycled identities: starts=%d err=%v", starts, err)
	}
}

type profileSuccessHistory struct {
	profileHistoryFixture
	successful []SIMIdentity
}

func (p *profileSuccessHistory) RememberIdentityRegistered(_ context.Context, id SIMIdentity) error {
	p.successful = append(p.successful, id)
	return nil
}

func TestProfileIdentitySuccessHistoryRequiresRegisteredIMS(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(strconv.FormatBool(registered), func(t *testing.T) {
			e := newFakeEnvironment()
			e.imsEvidence.Registered = registered
			historical := e.identity
			historical.IMSI = "204040123456789"
			historical.HomeMCC = "204"
			historical.HomeMNC = "04"
			history := &profileSuccessHistory{profileHistoryFixture: profileHistoryFixture{candidates: []SIMIdentity{historical, e.identity}}}
			d := discoveryDependencies(e)
			d.Discovery = history
			o, _ := New(d, Options{DeviceID: "profile"})
			defer o.Disable(context.Background())
			_, err := o.Enable(context.Background())
			want := 0
			if registered {
				want = 1
			}
			if len(history.successful) != want || registered != (err == nil) {
				t.Fatalf("success learned before registration: registered=%v count=%d err=%v", registered, len(history.successful), err)
			}
		})
	}
}

type profileSensitiveAT struct {
	profileIdentityAT
	sensitive    []string
	ordinaryFile bool
}

func (p *profileSensitiveAT) ExecuteAT(ctx context.Context, device, command string) (modem.Response, error) {
	if strings.HasPrefix(command, "AT+CRSM=176,28423,") {
		p.ordinaryFile = true
	}
	return p.profileIdentityAT.ExecuteAT(ctx, device, command)
}
func (p *profileSensitiveAT) ExecuteSensitiveAT(ctx context.Context, device, command string) (modem.Response, error) {
	p.sensitive = append(p.sensitive, command)
	return p.profileIdentityAT.ExecuteAT(ctx, device, command)
}

func TestProfileIdentityEFIMSIReadUsesSensitiveATBoundary(t *testing.T) {
	executor := &profileSensitiveAT{}
	adapter, _ := NewEC20Adapter(executor, EC20AdapterOptions{ReadProfileIdentities: true, AllowUnresolvedMNCLength: true})
	if _, err := adapter.ReadIdentity(context.Background(), "profile"); err != nil {
		t.Fatal(err)
	}
	if executor.ordinaryFile || len(executor.sensitive) != 2 {
		t.Fatal("encoded IMSI entered ordinary device error retention")
	}
}

func TestProfileIdentityPlanBudgetKeepsEveryIdentityAndLegalMncHome(t *testing.T) {
	e := newFakeEnvironment()
	history := &profileHistoryFixture{}
	for i := 0; i < 7; i++ {
		id := e.identity
		id.IMSI = fmt.Sprintf("999123%09d", i)
		id.HomeMCC = "999"
		id.HomeMNC = "12"
		history.candidates = append(history.candidates, id)
	}
	history.candidates = append(history.candidates, e.identity)
	history.options.ManualOnly = true
	for i := 0; i < 16; i++ {
		history.options.PreferredAddresses = append(history.options.PreferredAddresses, fmt.Sprintf("gateway-%d.example", i))
	}
	d := discoveryDependencies(e)
	d.Discovery = history
	o, _ := New(d, Options{DeviceID: "profile"})
	plans, _, err := o.profileEPDGPlan(context.Background(), e.identity)
	if err != nil || len(plans) != 64 {
		t.Fatalf("budget=%d err=%v", len(plans), err)
	}
	homes := map[string]map[string]bool{}
	for _, plan := range plans {
		if homes[plan.Identity.IMSI] == nil {
			homes[plan.Identity.IMSI] = map[string]bool{}
		}
		homes[plan.Identity.IMSI][plan.Identity.HomeMCC+plan.Identity.HomeMNC] = true
	}
	if len(homes) != 8 || !homes[e.identity.IMSI]["23415"] {
		t.Fatal("later current identity was starved")
	}
	for _, id := range history.candidates[:7] {
		if !homes[id.IMSI]["99912"] || !homes[id.IMSI]["999123"] {
			t.Fatal("legal MNC home was starved")
		}
	}
}

type profileSwitchAKA struct {
	fakeAKA
	checks     []SIMIdentity
	failSecond bool
}

func (p *profileSwitchAKA) CheckReady(ctx context.Context, id SIMIdentity) (AKAEvidence, error) {
	p.checks = append(p.checks, id)
	if p.failSecond && len(p.checks) == 2 {
		return AKAEvidence{}, ErrEC20IdentityChanged
	}
	return p.fakeAKA.CheckReady(ctx, id)
}

func TestProfileIdentityOnlyProvenPreChallengeRejectionAllowsNextIdentity(t *testing.T) {
	pure := &EPDGPreAKAIdentityError{Cause: ErrEAPAuthenticationRejected}
	for name, cause := range map[string]error{
		"raw":          ErrEAPAuthenticationRejected,
		"nil-proof":    &EPDGPreAKAIdentityError{},
		"mixed-proof":  &EPDGPreAKAIdentityError{Cause: errors.Join(ErrEAPAuthenticationRejected, errors.New("authentication failure"))},
		"mixed-branch": errors.Join(pure, ErrEAPAuthenticationRejected),
		"canceled":     errors.Join(pure, context.Canceled),
	} {
		if EPDGFallbackAllowed(cause) {
			t.Fatalf("invalid proof %s allowed fallback", name)
		}
	}
	for _, changeCard := range []bool{false, true} {
		t.Run(strconv.FormatBool(changeCard), func(t *testing.T) {
			e := newFakeEnvironment()
			historical := e.identity
			historical.IMSI = "204040123456789"
			historical.HomeMCC, historical.HomeMNC = "204", "04"
			d := discoveryDependencies(e)
			d.Discovery = &profileHistoryFixture{candidates: []SIMIdentity{historical, e.identity}}
			aka := &profileSwitchAKA{fakeAKA: fakeAKA{e}, failSecond: changeCard}
			d.AKA = aka
			var selected []string
			d.Tunnel = gatewayTunnelFunc(func(ctx context.Context, r TunnelRequest) (TunnelSession, error) {
				selected = append(selected, r.Identity.IMSI)
				if r.Identity.IMSI == historical.IMSI {
					return nil, pure
				}
				return fakeTunnelProvider{e}.Start(ctx, r)
			})
			o, _ := New(d, Options{DeviceID: "profile"})
			defer o.Disable(context.Background())
			_, err := o.Enable(context.Background())
			if len(aka.checks) != 2 || aka.checks[1].IMSI != e.identity.IMSI || aka.checks[1].ICCID != e.identity.ICCID {
				t.Fatalf("source was not freshly checked before identity switch: checks=%d", len(aka.checks))
			}
			if changeCard {
				if !errors.Is(err, ErrEC20IdentityChanged) || slices.Contains(selected, e.identity.IMSI) {
					t.Fatalf("changed card reached next network attempt: err=%v", err)
				}
			} else if err != nil || selected[len(selected)-1] != e.identity.IMSI {
				t.Fatalf("valid source could not use current identity: err=%v", err)
			}
		})
	}
}
