package ike

import "vocat/internal/vowifi"

// Called only on a decrypted, correlated IKE_AUTH reply. The caller must verify
// initial or final MSK AUTH on this startFamily invocation, never another SA.
// A true shape probe is not evidence until that current AUTH is verified.
func authenticatedNetworkRejection(items []payload, authenticated bool) *vowifi.EPDGNetworkRejection {
	if !authenticated {
		return nil
	}
	var cause uint16
	var scope vowifi.EPDGRejectionScope
	errors, timers := 0, 0
	timer := vowifi.IKEBackoffTimer{Status: vowifi.IKEBackoffMissing}
	for _, item := range payloadsOfType(items, payloadNotify) {
		kind, data, err := parseNotify(item)
		if err != nil {
			return nil
		}
		if kind < 16384 {
			errors++
			switch kind {
			case 10500: // NETWORK_FAILURE, not NO_APN_SUBSCRIPTION (9002).
				scope = vowifi.EPDGRejectionEndpoint
			case 9002:
				scope = vowifi.EPDGRejectionPLMNAPN
			case 9000, 9001, 9003, 9006, 11001, 11011:
				scope = vowifi.EPDGRejectionPLMN
			default:
				return nil
			}
			if item.Body[1] != 0 || len(data) != 0 {
				return nil
			}
			cause = kind
		}
		if kind == 41041 {
			timers++
			if timers == 1 && item.Body[1] == 0 {
				timer = vowifi.DecodeIKEBackoffTimer(data)
			} else {
				timer = vowifi.IKEBackoffTimer{Status: vowifi.IKEBackoffInvalid}
			}
		}
	}
	if errors != 1 {
		return nil
	}
	return &vowifi.EPDGNetworkRejection{NotifyType: cause, Scope: scope, Backoff: timer}
}
