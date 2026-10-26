package vowifi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"vocat/internal/pcsc"
)

type PCSCBindingResolver func(context.Context, string) (pcsc.Selector, string, error)

// PCSCAdapter uses a directly attached USB smart-card reader as the UICC for
// Wi-Fi Calling. It deliberately exposes no cellular-radio behaviour.
type PCSCAdapter struct {
	service  *pcsc.Service
	resolve  PCSCBindingResolver
	mu       sync.RWMutex
	bindings map[string]string
	options  SIMReaderOptions
}

var (
	_ SIMIdentityReader    = (*PCSCAdapter)(nil)
	_ SMSCenterReader      = (*PCSCAdapter)(nil)
	_ AKAProvider          = (*PCSCAdapter)(nil)
	_ PreferredAKAProvider = (*PCSCAdapter)(nil)
	_ RadioController      = (*PCSCAdapter)(nil)
)

func NewPCSCAdapter(service *pcsc.Service, resolver PCSCBindingResolver, options ...SIMReaderOptions) (*PCSCAdapter, error) {
	if service == nil || resolver == nil {
		return nil, errors.New("vocat: PC/SC service and reader resolver are required")
	}
	if len(options) > 1 {
		return nil, errors.New("vocat: only one SIM reader policy is allowed")
	}
	policy := SIMReaderOptions{}
	if len(options) == 1 {
		policy = options[0]
	}
	return &PCSCAdapter{service: service, resolve: resolver, bindings: make(map[string]string), options: policy}, nil
}

func (adapter *PCSCAdapter) ReadIdentity(ctx context.Context, deviceID string) (SIMIdentity, error) {
	selector, pin, err := adapter.resolve(ctx, strings.TrimSpace(deviceID))
	if err != nil {
		return SIMIdentity{}, err
	}
	var identity pcsc.Identity
	if adapter.options.AllowUnresolvedMNCLength {
		identity, err = adapter.service.ReadIdentityVerified(ctx, selector, pin)
	} else {
		identity, err = adapter.service.ReadIdentity(ctx, selector, pin)
	}
	if err != nil {
		return SIMIdentity{}, fmt.Errorf("read USB SIM identity: %w", err)
	}
	if len(identity.IMSI) < 5 {
		return SIMIdentity{}, errors.New("vocat: USB SIM reader returned an invalid IMSI")
	}
	mncLength := identity.MNCLength
	result := SIMIdentity{ICCID: identity.ICCID, IMSI: identity.IMSI, SMSC: identity.SMSC, SPN: identity.SPN}
	if mncLength != 2 && mncLength != 3 {
		if mcc, mnc, ok := assignedHomePLMN(identity.IMSI); ok {
			result.HomeMCC, result.HomeMNC = mcc, mnc
		} else if adapter.options.AllowUnresolvedMNCLength && validDigits(identity.IMSI, 6, 16) {
			result.HomeMCC, result.HomeMNC = identity.IMSI[:3], identity.IMSI[3:5]
			result.HomeMNCUncertain = true
		} else {
			return SIMIdentity{}, ErrEC20MNCUnavailable
		}
	} else {
		if len(identity.IMSI) < 3+mncLength {
			return SIMIdentity{}, errors.New("vocat: USB SIM IMSI is shorter than its EF_AD home PLMN")
		}
		result.HomeMCC, result.HomeMNC = identity.IMSI[:3], identity.IMSI[3:3+mncLength]
	}
	result = applyAssignedCarrierRoute(result)
	if ResolveCarrierProfile(result).RouteMCC != "" {
		result.HomeMNCUncertain = false
	}
	if err := result.validate(); err != nil {
		return SIMIdentity{}, err
	}
	if err := ctx.Err(); err != nil {
		return SIMIdentity{}, err
	}
	adapter.mu.Lock()
	adapter.bindings[result.ICCID] = strings.TrimSpace(deviceID)
	adapter.bindings[result.IMSI] = strings.TrimSpace(deviceID)
	adapter.mu.Unlock()
	return result, nil
}

func (adapter *PCSCAdapter) ReadSMSCenter(ctx context.Context, deviceID string) (string, error) {
	selector, pin, err := adapter.resolve(ctx, strings.TrimSpace(deviceID))
	if err != nil {
		return "", err
	}
	identity, err := adapter.service.ReadIdentity(ctx, selector, pin)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(identity.SMSC) == "" {
		return "", errors.New("vocat: USB SIM does not expose a service-centre address")
	}
	return identity.SMSC, nil
}

func (adapter *PCSCAdapter) CheckReady(ctx context.Context, identity SIMIdentity) (AKAEvidence, error) {
	selector, pin, err := adapter.resolve(ctx, adapter.deviceID(identity))
	if err != nil {
		return AKAEvidence{}, err
	}
	if err := adapter.verifySubscriber(ctx, selector, pin, identity); err != nil {
		return AKAEvidence{}, err
	}
	aid, err := adapter.service.CheckReady(ctx, selector, identity.ICCID, pin)
	if err != nil {
		return AKAEvidence{}, fmt.Errorf("check USB SIM AKA application: %w", err)
	}
	return AKAEvidence{Ready: true, Application: aid}, nil
}

func (adapter *PCSCAdapter) verifySubscriber(ctx context.Context, selector pcsc.Selector, pin string, expected SIMIdentity) error {
	if !adapter.options.AllowUnresolvedMNCLength {
		return nil
	}
	live, err := adapter.service.ReadIdentityVerified(ctx, selector, pin)
	if err != nil {
		return err
	}
	if live.ICCID != expected.ICCID || live.IMSI != expected.IMSI {
		return pcsc.ErrCardChanged
	}
	if (expected.HomeMNCUncertain || ResolveCarrierProfile(expected).RouteMCC == "") && (live.MNCLength == 2 || live.MNCLength == 3) && live.IMSI[3:3+live.MNCLength] != expected.HomeMNC {
		return pcsc.ErrCardChanged
	}
	return ctx.Err()
}

func (adapter *PCSCAdapter) deviceID(identity SIMIdentity) string {
	adapter.mu.RLock()
	defer adapter.mu.RUnlock()
	if id, ok := adapter.bindings[identity.ICCID]; ok && id != "" {
		return id
	}
	if id, ok := adapter.bindings[identity.IMSI]; ok && id != "" {
		return id
	}
	var candidate string
	for _, id := range adapter.bindings {
		if id == "" {
			continue
		}
		if candidate == "" {
			candidate = id
		} else if candidate != id {
			return ""
		}
	}
	return candidate
}

func (adapter *PCSCAdapter) Authenticate(ctx context.Context, identity SIMIdentity, challenge AKAChallenge) (AKAResult, error) {
	return adapter.AuthenticateWithPreference(ctx, identity, challenge, "")
}

func (adapter *PCSCAdapter) AuthenticateWithPreference(
	ctx context.Context,
	identity SIMIdentity,
	challenge AKAChallenge,
	preference string,
) (AKAResult, error) {
	selector, pin, err := adapter.resolve(ctx, adapter.deviceID(identity))
	if err != nil {
		return AKAResult{}, err
	}
	if err := adapter.verifySubscriber(ctx, selector, pin, identity); err != nil {
		return AKAResult{}, err
	}
	result, err := adapter.service.AuthenticateWithPreference(
		ctx,
		selector,
		identity.ICCID,
		pin,
		pcsc.AKAChallenge(challenge),
		preference,
	)
	if err != nil {
		if errors.Is(err, pcsc.ErrISIMUnavailable) || errors.Is(err, pcsc.ErrUSIMUnavailable) {
			return AKAResult{}, errors.Join(ErrEC20ApplicationAbsent, err)
		}
		if errors.Is(err, pcsc.ErrAKARejected) {
			return AKAResult{}, errors.Join(ErrEC20AKAMACFailure, err)
		}
		return AKAResult{}, fmt.Errorf("authenticate with USB SIM: %w", err)
	}
	return AKAResult(result), nil
}

func (*PCSCAdapter) Snapshot(context.Context, string) (RadioSnapshot, error) {
	return RadioSnapshot{OperatingMode: 4, PureAirplanePolicy: true}, nil
}
func (*PCSCAdapter) StopCellularData(context.Context, string) error       { return nil }
func (*PCSCAdapter) EnterVoWiFiRFOff(context.Context, string) error       { return nil }
func (*PCSCAdapter) Restore(context.Context, string, RadioSnapshot) error { return nil }
