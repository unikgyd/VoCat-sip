package vowifi

import "time"

type IKEBackoffTimerStatus string

const (
	IKEBackoffMissing     IKEBackoffTimerStatus = "missing"
	IKEBackoffInvalid     IKEBackoffTimerStatus = "invalid"
	IKEBackoffTimed       IKEBackoffTimerStatus = "timed"
	IKEBackoffDeactivated IKEBackoffTimerStatus = "deactivated"
)

// IKEBackoffTimer describes Tw3, not GPRS Timer or an extended periodic timer.
// A zero timed value, missing value and deactivated timer are distinct states.
type IKEBackoffTimer struct {
	Status   IKEBackoffTimerStatus
	Encoded  byte
	Duration time.Duration
}

func DecodeIKEBackoffTimer(data []byte) IKEBackoffTimer {
	if len(data) != 2 || data[0] != 1 {
		return IKEBackoffTimer{Status: IKEBackoffInvalid}
	}
	encoded := data[1]
	unit := encoded >> 5
	if unit == 7 {
		return IKEBackoffTimer{Status: IKEBackoffDeactivated, Encoded: encoded}
	}
	// TS 24.008 10.5.7.4a: unit 6's 320-hour exception is for extended
	// periodic timers; ordinary Tw3 uses its one-hour interpretation.
	scale := [...]time.Duration{600, 3600, 36000, 2, 30, 60, 3600}
	return IKEBackoffTimer{Status: IKEBackoffTimed, Encoded: encoded, Duration: scale[unit] * time.Duration(encoded&31) * time.Second}
}

type EPDGRejectionScope string

const (
	EPDGRejectionEndpoint EPDGRejectionScope = "epdg"
	EPDGRejectionPLMNAPN  EPDGRejectionScope = "plmn_apn"
	EPDGRejectionPLMN     EPDGRejectionScope = "plmn"
)

// EPDGNetworkRejection is issued by IKE only for a coherent error on the
// current initially authenticated SA. It is evidence, not a reconnect gate.
// The orchestrator must bind PLMN scopes to its actual candidate and card;
// neither the error's text nor an earlier successful SA supplies that proof.
type EPDGNetworkRejection struct {
	Cause      error
	NotifyType uint16
	Scope      EPDGRejectionScope
	Backoff    IKEBackoffTimer
	EPDG       string
	Endpoint   string
	APN        string
}

func (e *EPDGNetworkRejection) Error() string {
	if e == nil || e.Cause == nil {
		return "vowifi: authenticated ePDG rejection"
	}
	return e.Cause.Error()
}

func (e *EPDGNetworkRejection) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
