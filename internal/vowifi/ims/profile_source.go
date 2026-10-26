package ims

import "vocat/internal/vowifi"

// Missing/mutated source evidence stays unbound; never guess the current SIM.
func (session *Session) sourceIMSI() string {
	physical, err := vowifi.AKAIdentity(session.request.Identity)
	if err != nil {
		return ""
	}
	return physical.IMSI
}
