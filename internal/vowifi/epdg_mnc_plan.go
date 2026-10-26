package vowifi

import (
	"fmt"
	"sort"
)

// EPDGAttempt keeps an access endpoint and its authentication home inseparable.
// Discovery PLMNs are never wholesale substituted into a SIM identity.
type EPDGAttempt struct {
	Candidate EPDGCandidate
	Identity  SIMIdentity
}

// AuthenticationHomePLMNs preserves reader authority by default. Unknown MNC
// length is an explicit piece of evidence, not inferred from an absent field.
func AuthenticationHomePLMNs(identity SIMIdentity) ([]string, error) {
	if err := identity.validate(); err != nil {
		return nil, err
	}
	primary := identity.HomeMCC + identity.HomeMNC
	if !identity.HomeMNCUncertain {
		return []string{primary}, nil
	}
	if profile := ResolveCarrierProfile(identity); profile.RouteMCC != "" {
		return nil, fmt.Errorf("%w: unresolved MNC conflicts with explicit carrier home", ErrInvalidIdentity)
	}
	var result []string
	add := func(plmn string) {
		if plmn != identity.IMSI[:5] && plmn != identity.IMSI[:6] {
			return
		}
		for _, old := range result {
			if old == plmn {
				return
			}
		}
		result = append(result, plmn)
	}
	for _, plmn := range identity.DiscoveryPLMNs {
		add(plmn)
	}
	add(primary)
	add(identity.IMSI[:5])
	add(identity.IMSI[:6])
	return result, nil
}

func AuthenticationHomeAllowed(identity SIMIdentity, mcc, mnc string) bool {
	if !isNDigits(mcc, 3, 3) || !isNDigits(mnc, 2, 3) {
		return false
	}
	homes, err := AuthenticationHomePLMNs(identity)
	if err != nil {
		return false
	}
	for _, home := range homes {
		if home == mcc+mnc {
			return true
		}
	}
	return false
}

// BuildEPDGAttempts bounds the complete endpoint/realm plan, not just each
// individual home's address list. A standard home domain may select a legal
// MNC length; a foreign/manual domain can never invent an authentication home.
func BuildEPDGAttempts(identity SIMIdentity, options EPDGDiscoveryOptions) ([]EPDGAttempt, error) {
	homes, err := AuthenticationHomePLMNs(identity)
	if err != nil {
		return nil, err
	}
	var result []EPDGAttempt
	seen := map[string]bool{}
	for _, home := range homes {
		attemptIdentity := identity
		attemptIdentity.HomeMCC, attemptIdentity.HomeMNC = home[:3], home[3:]
		// Readers may have generated the initial default host before MNC length
		// was resolved. Regenerate that host, not an explicit custom endpoint.
		if identity.HomeMNCUncertain && identity.EPDG == standardEPDGHostname(identity.HomeMCC, identity.HomeMNC) {
			attemptIdentity.EPDG = standardEPDGHostname(attemptIdentity.HomeMCC, attemptIdentity.HomeMNC)
		}
		candidates, err := BuildEPDGCandidates(attemptIdentity, options)
		if err != nil {
			return nil, err
		}
		for _, candidate := range candidates {
			bound := attemptIdentity
			// Prefer the current home when two legal spellings pad to the same
			// domain (e.g. 00 / 000), otherwise infer only among legal prefixes.
			if candidate.Host != standardEPDGHostname(bound.HomeMCC, bound.HomeMNC) {
				for _, legal := range homes {
					if candidate.Host == standardEPDGHostname(legal[:3], legal[3:]) {
						bound.HomeMCC, bound.HomeMNC = legal[:3], legal[3:]
						break
					}
				}
			}
			// Success history is a host + realm pair, not merely a hostname.
			if candidate.Source == "last_registered" && options.LastSuccessful != nil {
				bound.HomeMCC, bound.HomeMNC = options.LastSuccessful.HomeMCC, options.LastSuccessful.HomeMNC
			}
			key := candidate.Host + "\n" + bound.HomeMCC + bound.HomeMNC
			if !seen[key] {
				seen[key] = true
				bound.DiscoveryPLMNs = append([]string(nil), bound.DiscoveryPLMNs...)
				result = append(result, EPDGAttempt{Candidate: candidate, Identity: bound})
			}
		}
	}
	priority := map[string]int{"last_registered": 0, "manual": 1, "configured": 2, "carrier": 3, "carrier_plmn": 4, "home_plmn": 5, "discovery_plmn": 6}
	sort.SliceStable(result, func(i, j int) bool {
		return priority[result[i].Candidate.Source] < priority[result[j].Candidate.Source]
	})
	return result[:min(len(result), 64)], nil
}
