package vowifi

import (
	"context"
	"errors"
	"sort"
	"strings"
)

const MaxProfileIdentities = 8

var ErrProfileIdentityBinding = errors.New("vowifi: authentication identity lost its verified physical SIM binding")

// Optional history returns an ordered, bounded plan for the verified current
// ICCID. It cannot change the reader's physical identity or AKA application.
type SIMIdentityHistory interface {
	IdentityCandidates(context.Context, SIMIdentity) ([]SIMIdentity, error)
}
type SIMIdentitySuccessStore interface {
	RememberIdentityRegistered(context.Context, SIMIdentity) error
}
type profileIdentitySource struct {
	live       SIMIdentity
	authorized SIMIdentity
}

func cloneProfileIdentity(id SIMIdentity) SIMIdentity {
	id.DiscoveryPLMNs = append([]string(nil), id.DiscoveryPLMNs...)
	id.profileIdentities = nil
	id.profileSource = nil
	return id
}

// CurrentProfileIdentities exposes only completed reader observations, never
// guessed identities. A different IMSI never inherits the live EF_AD length.
func CurrentProfileIdentities(live SIMIdentity) []SIMIdentity {
	result := []SIMIdentity{cloneProfileIdentity(live)}
	for _, candidate := range live.profileIdentities {
		if candidate.ICCID == live.ICCID && candidate.IMSI != live.IMSI {
			copy := cloneProfileIdentity(candidate)
			copy.SMSC = live.SMSC
			result = append(result, copy)
		}
	}
	return result[:min(len(result), MaxProfileIdentities)]
}

// ProfileIdentityFromHistory reconstructs a past observation as a tentative
// realm. Only a matching current reader observation carries current EF_AD.
func ProfileIdentityFromHistory(live SIMIdentity, imsi string) (SIMIdentity, error) {
	if !isNDigits(imsi, 6, 16) || strings.TrimSpace(live.ICCID) == "" {
		return SIMIdentity{}, ErrProfileIdentityBinding
	}
	for _, current := range CurrentProfileIdentities(live) {
		if current.IMSI == imsi {
			return current, nil
		}
	}
	id := cloneProfileIdentity(live)
	id.IMSI = imsi
	id.HomeMCC = imsi[:3]
	id.HomeMNC = imsi[3:5]
	id.HomeMNCUncertain = true
	id.HomeCountryCode = ""
	if id.EPDG == standardEPDGHostname(live.HomeMCC, live.HomeMNC) {
		id.EPDG = ""
	}
	id = applyAssignedCarrierRoute(id)
	if profile := ResolveCarrierProfile(id); profile.RouteMCC != "" {
		id.HomeMNCUncertain = false
	}
	return id, id.validate()
}

// AKAIdentity is the only mapping used at hardware/challenge and business
// source boundaries. Wire NAI/SIP identities still use the selected attempt.
func AKAIdentity(identity SIMIdentity) (SIMIdentity, error) {
	source := identity.profileSource
	if source == nil {
		return identity, nil
	}
	if identity.ICCID != source.live.ICCID || identity.IMSI != source.authorized.IMSI ||
		!AuthenticationHomeAllowed(source.authorized, identity.HomeMCC, identity.HomeMNC) {
		return SIMIdentity{}, ErrProfileIdentityBinding
	}
	return cloneProfileIdentity(source.live), nil
}

// ProfileAuthenticationIdentity prepares one locally approved observation or
// history entry. It is not a SIM-authentication grant: the physical provider
// must still recheck the original card before and after its AKA transaction.
func ProfileAuthenticationIdentity(live, selected SIMIdentity) (SIMIdentity, error) {
	if live.ICCID == "" || selected.ICCID != live.ICCID {
		return SIMIdentity{}, ErrProfileIdentityBinding
	}
	if selected.IMSI == live.IMSI {
		return cloneProfileIdentity(live), nil
	}
	id, err := ProfileIdentityFromHistory(live, selected.IMSI)
	if err != nil {
		return SIMIdentity{}, err
	}
	id.profileSource = &profileIdentitySource{live: cloneProfileIdentity(live), authorized: cloneProfileIdentity(id)}
	return id, nil
}

func (o *Orchestrator) profileEPDGPlan(ctx context.Context, live SIMIdentity) ([]EPDGAttempt, EPDGDiscoveryOptions, error) {
	candidates := CurrentProfileIdentities(live)
	if history, ok := o.deps.Discovery.(SIMIdentityHistory); ok {
		learned, err := history.IdentityCandidates(ctx, live)
		if ctx.Err() != nil {
			return nil, EPDGDiscoveryOptions{}, ctx.Err()
		}
		if err != nil {
			o.addWarning("SIM identity history is unavailable; using current verified reads")
		}
		if err == nil && len(learned) > 0 {
			candidates = learned
		}
	}
	if len(candidates) > MaxProfileIdentities {
		return nil, EPDGDiscoveryOptions{}, ErrProfileIdentityBinding
	}
	seen := map[string]bool{}
	var plans [][]EPDGAttempt
	var options EPDGDiscoveryOptions
	for _, candidate := range candidates {
		if candidate.ICCID != live.ICCID {
			return nil, options, ErrProfileIdentityBinding
		}
		if seen[candidate.IMSI] {
			continue
		}
		seen[candidate.IMSI] = true
		if err := candidate.validate(); err != nil {
			return nil, options, err
		}
		// The store controls ordering, not live authority. Recreate from the
		// current observations or the history's IMSI alone, not saved metadata.
		id := cloneProfileIdentity(live)
		if candidate.IMSI != live.IMSI {
			var err error
			id, err = ProfileAuthenticationIdentity(live, candidate)
			if err != nil {
				return nil, options, err
			}
		}
		current := EPDGDiscoveryOptions{}
		if o.deps.Discovery != nil {
			var err error
			current, err = o.deps.Discovery.LoadDiscovery(ctx, id)
			if err != nil {
				return nil, options, err
			}
		}
		options = current
		plan, err := BuildEPDGAttempts(id, current)
		if err != nil {
			return nil, options, err
		}
		plans = append(plans, plan)
	}
	attempts := limitProfileIdentityPlans(plans)
	if len(attempts) == 0 {
		return nil, options, errors.New("vowifi: no verified profile identity plan")
	}
	return attempts, options, nil
}

// Share the existing total bound without starving a later current identity.
// Within each identity preserve the leading attempt for each legal MNC home,
// then fill in normal endpoint priority. A single identity retains its plan.
func limitProfileIdentityPlans(plans [][]EPDGAttempt) []EPDGAttempt {
	if len(plans) == 0 {
		return nil
	}
	if len(plans) == 1 {
		return plans[0][:min(len(plans[0]), 64)]
	}
	budgets := make([]int, len(plans))
	left := 64
	for i, plan := range plans {
		budgets[i] = min(len(plan), 64/len(plans))
		left -= budgets[i]
	}
	for left > 0 {
		added := false
		for i, plan := range plans {
			if left > 0 && budgets[i] < len(plan) {
				budgets[i]++
				left--
				added = true
			}
		}
		if !added {
			break
		}
	}
	var result []EPDGAttempt
	for i, plan := range plans {
		chosen := map[int]bool{}
		homes := map[string]bool{}
		for index, attempt := range plan {
			home := attempt.Identity.HomeMCC + "/" + attempt.Identity.HomeMNC
			if !homes[home] && len(chosen) < budgets[i] {
				homes[home] = true
				chosen[index] = true
			}
		}
		for index := range plan {
			if len(chosen) >= budgets[i] {
				break
			}
			chosen[index] = true
		}
		indices := make([]int, 0, len(chosen))
		for index := range chosen {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		for _, index := range indices {
			result = append(result, plan[index])
		}
	}
	return result
}
