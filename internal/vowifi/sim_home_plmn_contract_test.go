package vowifi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"vocat/internal/modem"
)

func TestSIMHomePLMNContract(t *testing.T) {
	var contract struct {
		Version int
		CRSM    []struct {
			ID, Response string
			Accepted     bool
		} `json:"crsm"`
		EFAD []struct {
			ID, Data  string
			MNCLength *int `json:"mnc_length"`
		} `json:"ef_ad"`
		EHPLMN []struct {
			ID, Data string
			PLMNs    []string
		} `json:"ehplmn"`
	}
	raw, err := os.ReadFile("../../../contracts/sim-home-plmn-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &contract); err != nil || contract.Version != 1 {
		t.Fatalf("contract: %v", err)
	}
	for _, v := range contract.EFAD {
		t.Run("EFAD/"+v.ID, func(t *testing.T) {
			steps := []ec20TranscriptStep{{command: "AT+CRSM=176,28589,0,0,4", lines: []string{`+CRSM: 144,0,"` + v.Data + `"`}}}
			if v.MNCLength == nil {
				steps = append(steps, ec20TranscriptStep{command: "AT+CRSM=176,28589,0,0,0", lines: []string{`+CRSM: 144,0,"` + v.Data + `"`}})
			}
			x := &ec20Transcript{t: t, steps: steps}
			a, _ := NewEC20Adapter(x, EC20AdapterOptions{})
			got, err := a.readExplicitMNCLength(context.Background(), "fixture")
			if v.MNCLength == nil {
				if !errors.Is(err, ErrEC20MNCUnavailable) {
					t.Fatalf("length=%d err=%v", got, err)
				}
			} else if err != nil || got != *v.MNCLength {
				t.Fatalf("length=%d err=%v", got, err)
			}
			x.assertDone()
		})
	}
	for _, v := range contract.EHPLMN {
		t.Run("EHPLMN/"+v.ID, func(t *testing.T) {
			raw, err := hex.DecodeString(v.Data)
			if err != nil {
				t.Fatal(err)
			}
			got := parsePLMNListFromBytes(raw)
			if len(got) != len(v.PLMNs) || len(got) > 0 && !reflect.DeepEqual(got, v.PLMNs) {
				t.Fatalf("got %v want %v", got, v.PLMNs)
			}
		})
	}
	for _, v := range contract.CRSM {
		t.Run("CRSM/"+v.ID, func(t *testing.T) {
			got, err := parseCRSMData(modem.Response{Final: "OK", Lines: []string{v.Response}})
			if (err == nil) != v.Accepted || err == nil && len(got) != 4 {
				t.Fatalf("accepted=%v got=%x error=%v", v.Accepted, got, err)
			}
		})
	}
}

func TestEC20PLMNDiscoveryUsesSIMFileWithoutChangingAuthHome(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(map[bool]string{true: "EFAD", false: "equivalent-prefix"}[explicit], func(t *testing.T) {
			const imsi = "999123123456789"
			steps := identityTranscriptStepsWithoutEFAD(imsi)
			if explicit {
				steps = append(steps, ec20TranscriptStep{command: "AT+CRSM=176,28589,0,0,4", lines: []string{`+CRSM: 144,0,"00000003"`}})
			} else {
				steps = append(steps, ec20TranscriptStep{command: "AT+CRSM=176,28589,0,0,4", final: "ERROR", err: errors.New("unavailable")}, ec20TranscriptStep{command: "AT+CRSM=176,28589,0,0,0", final: "ERROR", err: errors.New("unavailable")})
			}
			// Foreign equivalent home first; only 999-123 prefixes this IMSI.
			steps = append(steps, ec20TranscriptStep{command: "AT+CRSM=176,28633,0,0,0", lines: []string{`+CRSM: 144,0,"02F44015F56699392102F440"`}}, ec20TranscriptStep{command: "AT+CIMI", lines: []string{imsi}}, ec20TranscriptStep{command: "AT+CCID", lines: []string{"+CCID: 8944101234567890123"}})
			x := &ec20Transcript{t: t, steps: steps}
			a, _ := NewEC20Adapter(x, EC20AdapterOptions{})
			id, err := a.ReadIdentity(context.Background(), "fixture")
			if err != nil {
				t.Fatal(err)
			}
			if id.IMSI != imsi || id.HomeMCC != "999" || id.HomeMNC != "123" {
				t.Fatalf("auth identity changed: %+v", id)
			}
			for _, plmn := range []string{"20404", "51566", "999123"} {
				if !containsPLMN(id.DiscoveryPLMNs, plmn) {
					t.Fatalf("lost %s in %v", plmn, id.DiscoveryPLMNs)
				}
			}
			candidates, err := BuildEPDGCandidates(id, EPDGDiscoveryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range []string{"mnc004.mcc204.", "mnc066.mcc515.", "mnc123.mcc999."} {
				found := false
				for _, c := range candidates {
					found = found || strings.Contains(c.Host, part)
				}
				if !found {
					t.Fatalf("lost candidate %s: %v", part, candidates)
				}
			}
			x.assertDone()
		})
	}
}

func containsPLMN(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func TestEC20PLMNOptionalFileFailureAndBoundedReadFallback(t *testing.T) {
	for _, v := range []struct {
		name  string
		reads []ec20TranscriptStep
		want  string
	}{
		{"read-length-fallback", []ec20TranscriptStep{
			{command: "AT+CRSM=176,28633,0,0,0", final: "ERROR", err: errors.New("unsupported length")},
			{command: "AT+CRSM=176,28633,0,0,12", lines: []string{`+CRSM: 144,0,"15F56602F440FFFFFFFFFFFF"`}},
		}, "51566"},
		{"optional-not-present", []ec20TranscriptStep{
			{command: "AT+CRSM=176,28633,0,0,0", lines: []string{`+CRSM: 106,130`}},
			{command: "AT+CRSM=176,28633,0,0,12", lines: []string{`+CRSM: 106,130`}},
		}, ""},
		{"empty-no-redundant-read", []ec20TranscriptStep{
			{command: "AT+CRSM=176,28633,0,0,0", lines: []string{`+CRSM: 144,0,"FFFFFF"`}},
		}, ""},
	} {
		t.Run(v.name, func(t *testing.T) {
			const imsi = "999123123456789"
			steps := identityTranscriptStepsWithoutEFAD(imsi)
			steps = append(steps, ec20TranscriptStep{command: "AT+CRSM=176,28589,0,0,4", lines: []string{`+CRSM: 144,0,"00000003"`}})
			steps = append(steps, v.reads...)
			steps = append(steps, ec20TranscriptStep{command: "AT+CIMI", lines: []string{imsi}}, ec20TranscriptStep{command: "AT+CCID", lines: []string{"+CCID: 8944101234567890123"}})
			x := &ec20Transcript{t: t, steps: steps}
			a, _ := NewEC20Adapter(x, EC20AdapterOptions{})
			id, err := a.ReadIdentity(context.Background(), "fixture")
			if err != nil {
				t.Fatal(err)
			}
			if id.HomeMCC != "999" || id.HomeMNC != "123" {
				t.Fatal("optional file replaced verified home")
			}
			if v.want != "" && !containsPLMN(id.DiscoveryPLMNs, v.want) {
				t.Fatal("fallback candidate missing")
			}
			x.assertDone()
		})
	}
}

func TestEC20PLMNProfileChangeDoesNotPublishBinding(t *testing.T) {
	for _, v := range []struct{ name, imsi, iccid string }{
		{"imsi-changed", "999123999999999", "8944101234567890123"},
		{"iccid-changed", "999123123456789", "8944101234567890999"},
		{"imsi-invalid", "garbled", "8944101234567890123"},
	} {
		t.Run(v.name, func(t *testing.T) {
			steps := identityTranscriptStepsWithoutEFAD("999123123456789")
			steps = append(steps, ec20TranscriptStep{command: "AT+CRSM=176,28589,0,0,4", lines: []string{`+CRSM: 144,0,"00000003"`}}, ec20TranscriptStep{command: "AT+CRSM=176,28633,0,0,0", lines: []string{`+CRSM: 144,0,"15F566"`}}, ec20TranscriptStep{command: "AT+CIMI", lines: []string{v.imsi}}, ec20TranscriptStep{command: "AT+CCID", lines: []string{"+CCID: " + v.iccid}})
			x := &ec20Transcript{t: t, steps: steps}
			a, _ := NewEC20Adapter(x, EC20AdapterOptions{})
			id, err := a.ReadIdentity(context.Background(), "fixture")
			if !errors.Is(err, ErrEC20IdentityChanged) || id.ICCID != "" || len(a.bindings) != 0 {
				t.Fatalf("published changed identity: %+v %v", id, err)
			}
			x.assertDone()
		})
	}
}

func TestEC20PLMNCancellationNeverBecomesOptionalFileFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	e := &ec20PSIExecutor{run: func(_ context.Context, _ string, command string) (modem.Response, error) {
		calls++
		lines := map[string]string{"AT+CPIN?": "+CPIN: READY", "AT+CIMI": "999123123456789", "AT+CCID": "+CCID: 8944101234567890123", "AT+CGSN": "867530912345678", "AT+CRSM=176,28589,0,0,4": `+CRSM: 144,0,"00000003"`}
		if command == "AT+CRSM=176,28633,0,0,0" {
			cancel()
			return modem.Response{}, context.Canceled
		}
		line, ok := lines[command]
		if !ok {
			t.Fatalf("cancellation issued extra command %s", command)
		}
		return modem.Response{Final: "OK", Lines: []string{line}}, nil
	}}
	a, _ := NewEC20Adapter(e, EC20AdapterOptions{})
	if _, err := a.ReadIdentity(ctx, "fixture"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if calls != 6 || len(a.bindings) != 0 {
		t.Fatalf("calls=%d bindings=%d", calls, len(a.bindings))
	}
}

func TestEC20PLMNDiscoveryLimitDoesNotHideAmbiguousAuthPrefix(t *testing.T) {
	// The 17th unique entry still participates in authentication-home validation,
	// even though only 16 discovery hints may be retained for this identity.
	var encoded strings.Builder
	encoded.WriteString("99F921") // 999-12
	for i := 0; i < 15; i++ {
		fmt.Fprintf(&encoded, "%02X%02X%02X", 0x21, 0xf3, byte(i/10)<<4|byte(i%10))
	}
	encoded.WriteString("993921") // 999-123
	steps := identityTranscriptStepsWithoutEFAD("999123123456789")
	steps = append(steps, ec20TranscriptStep{command: "AT+CRSM=176,28589,0,0,4", lines: []string{`+CRSM: 144,0,"000000FF"`}}, ec20TranscriptStep{command: "AT+CRSM=176,28589,0,0,0", lines: []string{`+CRSM: 144,0,"000000FF"`}}, ec20TranscriptStep{command: "AT+CRSM=176,28633,0,0,0", lines: []string{`+CRSM: 144,0,"` + encoded.String() + `"`}})
	x := &ec20Transcript{t: t, steps: steps}
	a, _ := NewEC20Adapter(x, EC20AdapterOptions{})
	if _, err := a.ReadIdentity(context.Background(), "fixture"); !errors.Is(err, ErrEC20MNCUnavailable) {
		t.Fatalf("ambiguous length accepted: %v", err)
	}
	x.assertDone()
}

func TestEC20PLMNRejectsUnrelatedOrAmbiguousAuthenticationHome(t *testing.T) {
	for _, encoded := range []string{"02F44015F566", "99F921993921"} {
		t.Run(encoded, func(t *testing.T) {
			steps := identityTranscriptStepsWithoutEFAD("999123123456789")
			steps = append(steps, ec20TranscriptStep{command: "AT+CRSM=176,28589,0,0,4", lines: []string{`+CRSM: 144,0,"000000FF"`}}, ec20TranscriptStep{command: "AT+CRSM=176,28589,0,0,0", lines: []string{`+CRSM: 144,0,"000000FF"`}}, ec20TranscriptStep{command: "AT+CRSM=176,28633,0,0,0", lines: []string{`+CRSM: 144,0,"` + encoded + `"`}})
			x := &ec20Transcript{t: t, steps: steps}
			a, _ := NewEC20Adapter(x, EC20AdapterOptions{})
			if _, err := a.ReadIdentity(context.Background(), "fixture"); !errors.Is(err, ErrEC20MNCUnavailable) {
				t.Fatalf("expected unavailable MNC, got %v", err)
			}
			if len(a.bindings) != 0 {
				t.Fatal("unverified identity became an AKA binding")
			}
			x.assertDone()
		})
	}
}
