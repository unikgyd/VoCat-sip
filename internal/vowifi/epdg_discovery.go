package vowifi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

type EPDGDiscoveryOptions struct {
	GeographicDNSFallback bool           `json:"geographic_dns_fallback"`
	PreferredAddresses    []string       `json:"preferred_addresses"`
	AdditionalPLMNs       []string       `json:"additional_plmns"`
	ManualOnly            bool           `json:"manual_only"`
	LastSuccessful        *EPDGSelection `json:"-"`
}
type EPDGCandidate struct {
	Host   string `json:"host"`
	Source string `json:"source"`
	PLMN   string `json:"plmn,omitempty"`
}
type EPDGSelection struct {
	Host        string    `json:"host"`
	HomeMCC     string    `json:"home_mcc"`
	HomeMNC     string    `json:"home_mnc"`
	SucceededAt time.Time `json:"succeeded_at"`
}

// The lookup/persistence layer is optional. It cannot supply or replace an IMSI
// or AKA identity; only complete IMS registration is eligible for remembering.
type EPDGDiscoveryStore interface {
	LoadDiscovery(context.Context, SIMIdentity) (EPDGDiscoveryOptions, error)
	RememberRegistered(context.Context, SIMIdentity, EPDGCandidate) error
}
type EPDGAccessError struct{ Cause error }

func (e *EPDGAccessError) Error() string { return e.Cause.Error() }
func (e *EPDGAccessError) Unwrap() error { return e.Cause }

// Only an IKE provider's verified EAP-Failure before ANY AKA challenge, after
// successful local teardown. Raw EAP/authentication failures never qualify.
type EPDGPreAKAIdentityError struct{ Cause error }

func (e *EPDGPreAKAIdentityError) Error() string {
	if e == nil || e.Cause == nil {
		return "vowifi: identity rejected before AKA challenge"
	}
	return e.Cause.Error()
}
func (e *EPDGPreAKAIdentityError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
func EPDGFallbackAllowed(err error) bool {
	if errors.Is(err, ErrIKERetryBlocked) || errors.Is(err, ErrIKERetryStore) || errors.Is(err, ErrIKERetryBinding) {
		return false
	}
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrResponderAUTHRequired) {
		return false
	}
	if everyErrorCause(err, func(cause error) bool {
		proof, ok := cause.(*EPDGPreAKAIdentityError)
		return ok && proof != nil && everyErrorCause(proof.Cause, func(leaf error) bool { return leaf == ErrEAPAuthenticationRejected })
	}) {
		return true
	}
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrEAPAuthenticationRejected) || errors.Is(err, ErrResponderAUTHRequired) {
		return false
	}
	return everyErrorCause(err, func(cause error) bool {
		access, ok := cause.(*EPDGAccessError)
		return ok && access != nil && access.Cause != nil
	})
}

var epdgDNSName = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)*[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
var epdgPLMN = regexp.MustCompile(`^[0-9]{3}-?[0-9]{2,3}$`)

func NormalizeEPDGHost(host string) (string, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if ip := net.ParseIP(host); ip != nil {
		if !UsableEPDGAddress(ip) {
			return "", errors.New("ePDG address must be usable unicast")
		}
		return ip.String(), nil
	}
	if len(host) == 0 || len(host) > 253 || !epdgDNSName.MatchString(host) || strings.Trim(host, "0123456789.") == "" {
		return "", errors.New("ePDG requires a hostname or IP, without scheme, port, credentials or path")
	}
	return strings.ToLower(host), nil
}
func UsableEPDGAddress(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		return v4[0] < 224 && v4[0] != 0
	}
	return ip.To16() != nil
}
func (o EPDGDiscoveryOptions) Normalize() (EPDGDiscoveryOptions, error) {
	result := o
	result.PreferredAddresses = []string{}
	result.AdditionalPLMNs = []string{}
	if len(o.PreferredAddresses) > 16 || len(o.AdditionalPLMNs) > 16 {
		return result, errors.New("at most 16 ePDG hosts and 16 discovery PLMNs are allowed")
	}
	seen := map[string]bool{}
	for _, host := range o.PreferredAddresses {
		host, err := NormalizeEPDGHost(host)
		if err != nil {
			return result, err
		}
		if !seen[host] {
			seen[host] = true
			result.PreferredAddresses = append(result.PreferredAddresses, host)
		}
	}
	seen = map[string]bool{}
	for _, value := range o.AdditionalPLMNs {
		if !epdgPLMN.MatchString(strings.TrimSpace(value)) {
			return result, errors.New("discovery PLMN requires MCC-MNC or five/six digits")
		}
		value = strings.ReplaceAll(strings.TrimSpace(value), "-", "")
		if !isNDigits(value, 5, 6) {
			return result, errors.New("discovery PLMN requires MCC-MNC or five/six digits")
		}
		if !seen[value] {
			seen[value] = true
			result.AdditionalPLMNs = append(result.AdditionalPLMNs, value)
		}
	}
	if result.ManualOnly && len(result.PreferredAddresses) == 0 {
		return result, errors.New("manual-only ePDG discovery requires an address")
	}
	return result, nil
}
func BuildEPDGCandidates(identity SIMIdentity, options EPDGDiscoveryOptions) ([]EPDGCandidate, error) {
	if err := identity.validate(); err != nil {
		return nil, err
	}
	options, err := options.Normalize()
	if err != nil {
		return nil, err
	}
	result := []EPDGCandidate{}
	seen := map[string]bool{}
	add := func(host, source, plmn string) {
		normalized, err := NormalizeEPDGHost(host)
		if err == nil && !seen[normalized] && len(result) < 64 {
			seen[normalized] = true
			result = append(result, EPDGCandidate{normalized, source, plmn})
		}
	}
	manual := map[string]bool{}
	for _, host := range options.PreferredAddresses {
		manual[host] = true
	}
	if last := options.LastSuccessful; last != nil && last.HomeMCC == identity.HomeMCC && last.HomeMNC == identity.HomeMNC && (!options.ManualOnly || manual[last.Host]) {
		add(last.Host, "last_registered", "")
	}
	for _, host := range options.PreferredAddresses {
		add(host, "manual", "")
	}
	if options.ManualOnly {
		return result, nil
	}
	if identity.EPDG != "" {
		if _, err := NormalizeEPDGHost(identity.EPDG); err != nil {
			return nil, err
		}
		add(identity.EPDG, "configured", "")
	}
	profile := ResolveCarrierProfile(identity)
	if profile.EPDG != "" {
		add(profile.EPDG, "carrier", "")
	}
	if profile.RouteMCC != "" {
		add(standardEPDGHostname(profile.RouteMCC, profile.RouteMNC), "carrier_plmn", profile.RouteMCC+profile.RouteMNC)
	}
	add(standardEPDGHostname(identity.HomeMCC, identity.HomeMNC), "home_plmn", identity.HomeMCC+identity.HomeMNC)
	for _, plmn := range append(append([]string{}, identity.DiscoveryPLMNs...), options.AdditionalPLMNs...) {
		if isNDigits(plmn, 5, 6) {
			add(standardEPDGHostname(plmn[:3], plmn[3:]), "discovery_plmn", plmn)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("vowifi: no usable ePDG candidate")
	}
	return result, nil
}
