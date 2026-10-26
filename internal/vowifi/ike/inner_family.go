package ike

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"

	"vocat/internal/vowifi"
)

type innerFamily uint8

const (
	innerDual innerFamily = iota
	innerIPv6
	innerIPv4
)

func (family innerFamily) String() string { return [...]string{"dual", "ipv6", "ipv4"}[family] }

// Issued only by start after a precise protocol/configuration failure AND its
// pending DELETE and local transport cleanup have returned successfully.
type innerFamilyRetryError struct {
	cause    error
	endpoint string
	legacy   bool
	preEAP   bool
}

func (err *innerFamilyRetryError) Error() string { return err.cause.Error() }
func (err *innerFamilyRetryError) Unwrap() error { return err.cause }

func (provider *Provider) startInnerFamilies(ctx context.Context, request vowifi.TunnelRequest) (vowifi.TunnelSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var failures []error
	pinnedEndpoint := ""
	forceLegacy := false
	identityRetrySafe := true
	for _, family := range []innerFamily{innerDual, innerIPv6, innerIPv4} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		session, err := provider.startFamily(ctx, request, forceLegacy, family, pinnedEndpoint)
		// Legacy proposal recovery is an IKE_SA_INIT policy, never an AUTH or CP
		// downgrade. Once selected, retain that proposal for this family plan.
		var access *vowifi.EPDGAccessError
		if err != nil && pinnedEndpoint == "" && !forceLegacy && provider.config.AutoProposalFallback &&
			vowifi.ResolveCarrierProfile(request.Identity).ID == vowifi.CarrierProfileStandard &&
			errors.As(err, &access) && retryableLegacyProposal(err) && ctx.Err() == nil {
			forceLegacy = true
			provider.config.Logger.Warn("IKE initial proposal rejected; trying bounded legacy fallback", "inner_family", family.String())
			session, err = provider.startFamily(ctx, request, true, family, pinnedEndpoint)
		}
		if err == nil {
			return session, nil
		}
		if _, ok := err.(*vowifi.EPDGPreAKAIdentityError); ok && identityRetrySafe && ctx.Err() == nil {
			return nil, fmt.Errorf("%s after only pre-EAP family attempts: %w", family, err)
		}
		failures = append(failures, fmt.Errorf("%s: %w", family, err))
		proof, ok := err.(*innerFamilyRetryError)
		if !ok || ctx.Err() != nil {
			return nil, errors.Join(failures...)
		}
		identityRetrySafe = identityRetrySafe && proof.preEAP
		if pinnedEndpoint == "" {
			pinnedEndpoint = proof.endpoint
		}
		forceLegacy = proof.legacy
		if family != innerIPv4 {
			provider.config.Logger.Info("IKE inner configuration unusable; trying next family on the same endpoint", "inner_family", family.String(), "endpoint", pinnedEndpoint)
		}
	}
	return nil, fmt.Errorf("ike: inner address-family attempts exhausted: %w", errors.Join(failures...))
}

func innerTrafficSelectors(kind uint8, family innerFamily) payload {
	if family == innerDual {
		return dualStackTrafficSelectors(kind)
	}
	result := anyTrafficSelector(family == innerIPv6)
	result.Type = kind
	return result
}

// A protected first response can suggest a different request shape, but is not
// authentication evidence. Do not consume AKA, bypass AUTH or switch identity.
func initialFamilyRejection(items []payload) bool {
	found := false
	for _, item := range items {
		switch item.Type {
		case payloadNotify:
			kind, data, err := parseNotify(item)
			// RFC 7296 3.10: Protocol ID MUST be ignored when the SPI is
			// empty. These errors have no SPI; still reject nonempty SPI/data.
			if err != nil || item.Body[1] != 0 || kind == 41041 {
				return false
			}
			if kind == 36 || kind == 38 {
				if len(data) != 0 {
					return false
				}
				found = true
			} else if kind < 16384 {
				return false
			}
		case payloadIDr:
			if validateFQDNIDr(item, "", "initial responder") != nil {
				return false
			}
		case payloadAuth, payloadCert:
		default:
			return false
		}
	}
	return found
}

func validateInnerSelectors(selected, offered []trafficSelector) error {
	if len(selected) == 0 {
		return errors.New("ike: empty negotiated traffic selectors")
	}
	for _, actual := range selected {
		if len(actual.StartIP) == net.IPv6len && actual.StartIP.To4() != nil ||
			len(actual.EndIP) == net.IPv6len && actual.EndIP.To4() != nil {
			return errors.New("ike: IPv4-mapped IPv6 traffic selectors are not supported")
		}
		_, start, end, valid := normalizeIPRange(actual.StartIP, actual.StartIP, actual.EndIP)
		if !valid || bytes.Compare(start, end) > 0 || actual.StartPort > actual.EndPort {
			return errors.New("ike: invalid negotiated traffic selector range")
		}
		contained := false
		for _, allowed := range offered {
			if (allowed.IPProtocol == 0 || allowed.IPProtocol == actual.IPProtocol) &&
				actual.StartPort >= allowed.StartPort && actual.EndPort <= allowed.EndPort &&
				ipWithinRange(actual.StartIP, allowed.StartIP, allowed.EndIP) && ipWithinRange(actual.EndIP, allowed.StartIP, allowed.EndIP) {
				contained = true
				break
			}
		}
		if !contained {
			return errors.New("ike: responder widened traffic selectors beyond the requested inner family")
		}
	}
	return nil
}

func validateSelectedESP(selected proposal, offered proposal) error {
	if selected.Number != offered.Number || selected.Protocol != protocolESP || len(selected.SPI) != 4 || bytes.Equal(selected.SPI, make([]byte, 4)) {
		return errors.New("ike: responder selected an unoffered ESP proposal or zero SPI")
	}
	for _, actual := range selected.Transforms {
		found := false
		for _, allowed := range offered.Transforms {
			if actual == allowed {
				found = true
				break
			}
		}
		if !found {
			return errors.New("ike: responder selected an unoffered ESP transform")
		}
	}
	return nil
}

func usableInnerIP(ip net.IP) bool {
	return (len(ip) == net.IPv4len || len(ip) == net.IPv6len && ip.To4() == nil) &&
		!ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.Equal(net.IPv4bcast)
}

// Malformed or policy-violating data is fatal. Only an otherwise parsed CP
// missing a coherent UE/P-CSCF family is a family-compatibility result.
func validateInnerConfiguration(network networkConfiguration, family innerFamily, local, remote []trafficSelector) (networkConfiguration, bool, error) {
	if family == innerIPv6 && network.LocalIPv4 != nil || family == innerIPv4 && network.LocalIPv6 != nil {
		return network, false, errors.New("ike: CP assigned an unrequested inner address family")
	}
	for _, address := range []net.IP{network.LocalIPv4, network.LocalIPv6} {
		if address != nil && !usableInnerIP(address) {
			return network, false, errors.New("ike: CP assigned an invalid inner address")
		}
	}
	for _, peer := range network.PCSCF {
		if !usableInnerIP(peer) {
			return network, false, errors.New("ike: CP supplied an invalid P-CSCF")
		}
	}
	for _, address := range []net.IP{network.LocalIPv4, network.LocalIPv6} {
		if address == nil {
			continue
		}
		allowed := false
		for _, selector := range local {
			if (selector.IPProtocol == 0 || selector.IPProtocol == 17) && ipWithinRange(address, selector.StartIP, selector.EndIP) {
				allowed = true
				break
			}
		}
		if !allowed {
			return network, false, errors.New("ike: assigned inner address is outside negotiated UDP selectors")
		}
	}
	if network.LocalIPv4 == nil && network.LocalIPv6 == nil {
		return network, true, errors.New("ike: responder did not assign an inner IP address")
	}
	network.PCSCF = pcscfForAssignedFamilies(network.PCSCF, network.LocalIPv4 != nil, network.LocalIPv6 != nil)
	if len(network.PCSCF) == 0 {
		return network, true, errors.New("ike: responder did not provide a P-CSCF matching an assigned address family")
	}
	for _, peer := range network.PCSCF {
		if !endpointAllowed(peer, 17, 5060, remote) {
			return network, false, errors.New("ike: P-CSCF is outside negotiated UDP selectors")
		}
	}
	return network, false, nil
}
