package vowifi

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Exercise the actual AT/QMI identity, application selection and AKA adapters,
// not just an independent fake SIM and fake radio at the orchestrator boundary.
type policyAKATunnel struct{ fakeTunnelProvider }

func (p policyAKATunnel) Start(ctx context.Context, req TunnelRequest) (TunnelSession, error) {
	result, err := req.AKA.Authenticate(ctx, req.Identity, AKAChallenge{})
	if err != nil {
		return nil, err
	}
	if len(result.RES) != 8 || len(result.CK) != 16 || len(result.IK) != 16 {
		return nil, errors.New("adapter did not return the synthetic AKA result")
	}
	return p.fakeTunnelProvider.Start(ctx, req)
}

type coexistNativeController struct{ readerHomeNativeController }

func (*coexistNativeController) NativeQMIRadioSnapshot(context.Context, string) (int, bool, error) {
	panic("coexist identity/AKA must not acquire a radio checkpoint")
}
func (*coexistNativeController) StopNativeQMICellularData(context.Context, string) error {
	panic("coexist identity/AKA must not stop cellular data")
}
func (*coexistNativeController) SetNativeQMIRadioOff(context.Context, string, bool) error {
	panic("coexist identity/AKA must not set RF mode")
}

func exerciseCoexistAdapter(t *testing.T, adapter interface {
	SIMIdentityReader
	AKAProvider
	RadioController
}) {
	t.Helper()
	env := newFakeEnvironment()
	o, err := New(Dependencies{
		SIM: adapter, AKA: adapter, Radio: adapter, Proxy: fakeProxy{env},
		Tunnel: policyAKATunnel{fakeTunnelProvider{env}}, IMS: fakeIMSProvider{env}, Phones: fakePhones{env},
	}, Options{DeviceID: "same-physical-modem", ResolveRadioPolicy: func(context.Context) (RadioPolicy, error) { return RadioPolicyCoexist, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = o.Disable(context.Background()) })
	state, err := o.Enable(context.Background())
	if err != nil || !state.IMSReady || state.RadioPolicy != RadioPolicyCoexist {
		t.Fatalf("coexist with actual adapter: %+v, %v", state, err)
	}
	if _, err := o.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCoexistWiFiNativeQMIIdentityAndAKAWithoutRadioIO(t *testing.T) {
	controller := &coexistNativeController{readerHomeNativeController: readerHomeNativeController{known: true}}
	adapter, err := NewNativeQMIAdapter(controller, nil)
	if err != nil {
		t.Fatal(err)
	}
	exerciseCoexistAdapter(t, adapter)
	if controller.auths != 1 || controller.reads < 3 {
		t.Fatalf("QMI identity/AKA adapter was bypassed: reads=%d, auths=%d", controller.reads, controller.auths)
	}
}

func TestCoexistWiFiEC20IdentityAndAKAWithoutRadioIO(t *testing.T) {
	apdu := strings.ToUpper(hex.EncodeToString(buildUSIMAuthenticateAPDU(AKAChallenge{})))
	reply := strings.ToUpper(hex.EncodeToString(successfulUSIMResponse()))
	transcript := &ec20Transcript{t: t, steps: append(identityTranscriptSteps("234150123456789"),
		ec20TranscriptStep{command: "AT+CCID", lines: []string{"+CCID: 8944101234567890123"}},
		ec20TranscriptStep{command: "AT+CUAD", lines: []string{`+CUAD: 22,"61094F07A0000000871002"`}},
		ec20TranscriptStep{command: `AT+CCHO="A0000000871002"`, lines: []string{"+CCHO: 1"}},
		ec20TranscriptStep{command: "AT+CCHC=1"},
		ec20TranscriptStep{command: "AT+CSCA?", lines: []string{`+CSCA: "+447785016005",145`}},
		ec20TranscriptStep{command: "AT+CCID", lines: []string{"+CCID: 8944101234567890123"}},
		ec20TranscriptStep{command: `AT+CCHO="A0000000871002"`, lines: []string{"+CCHO: 1"}},
		ec20TranscriptStep{command: fmt.Sprintf(`AT+CGLA=1,%d,"%s"`, len(apdu), apdu), sensitive: true,
			lines: []string{fmt.Sprintf(`+CGLA: %d,"%s"`, len(reply), reply)}},
		ec20TranscriptStep{command: "AT+CCHC=1"},
	)}
	adapter, err := NewEC20Adapter(transcript, EC20AdapterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	exerciseCoexistAdapter(t, adapter)
	transcript.assertDone() // Any CFUN/CGACT/CGATT command fails this transcript.
}
