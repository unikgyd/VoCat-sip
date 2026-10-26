package ims

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"time"

	"vocat/internal/vowifi"
)

var ErrCellularAccess = errors.New("ims: owned cellular IMS bearer verification failed")

type imsAccessEvidence struct {
	LocalIPv4 string
	LocalIPv6 string
	PCSCF     []string
	PANI      string
}

func (provider *Provider) accessEvidence(ctx context.Context, request vowifi.IMSRequest) (imsAccessEvidence, error) {
	if request.Cellular != nil {
		identity := request.Identity
		_, ownsSecurity := request.Cellular.(vowifi.CellularIMSSecurity)
		if request.Tunnel != nil || !digitsBetween(identity.ICCID, 18, 20) ||
			!digitsBetween(identity.IMSI, 5, 16) || provider.config.SecurityMode != SecurityRequired || !ownsSecurity {
			return imsAccessEvidence{}, ErrCellularAccess
		}
		return verifiedCellularEvidence(ctx, request)
	}
	if request.Tunnel == nil {
		return imsAccessEvidence{}, errors.New("ims: tunnel or owned cellular bearer is required")
	}
	tunnel := request.Tunnel.Evidence()
	if !tunnel.Established {
		return imsAccessEvidence{}, vowifi.ErrTunnelNotEstablished
	}
	return imsAccessEvidence{LocalIPv4: tunnel.LocalIPv4, LocalIPv6: tunnel.LocalIPv6,
		PCSCF: append([]string(nil), tunnel.PCSCF...)}, nil
}

func verifiedCellularEvidence(ctx context.Context, request vowifi.IMSRequest) (imsAccessEvidence, error) {
	verifyContext, cancel := context.WithTimeout(ctx, cellularVerificationBudget(request.Cellular))
	defer cancel()
	if err := request.Cellular.Verify(verifyContext, request.Identity); err != nil || verifyContext.Err() != nil {
		// Bearer/provider errors may contain real SIM identifiers. Do not surface
		// them through the IMS failure channel or a web diagnostic.
		return imsAccessEvidence{}, ErrCellularAccess
	}
	evidence := request.Cellular.Evidence()
	pani, err := evidence.Cell.PAccessNetworkInfo()
	if err != nil || !evidence.Established || len(evidence.PCSCF) == 0 || len(evidence.PCSCF) > 8 {
		return imsAccessEvidence{}, ErrCellularAccess
	}
	v4, v6 := evidence.LocalIPv4, evidence.LocalIPv6
	if v4 == "" && v6 == "" {
		return imsAccessEvidence{}, ErrCellularAccess
	}
	for family, value := range map[int]string{4: v4, 6: v6} {
		if value == "" {
			continue
		}
		ip := net.ParseIP(value)
		if !usableBearerIP(ip) || (ip.To4() != nil) != (family == 4) {
			return imsAccessEvidence{}, ErrCellularAccess
		}
	}
	for _, value := range evidence.PCSCF {
		endpoint, _, err := parsePCSCF(value, 5060)
		ip := net.ParseIP(endpoint.host)
		if err != nil || !usableBearerIP(ip) ||
			(ip.To4() != nil && v4 == "") || (ip.To4() == nil && v6 == "") {
			return imsAccessEvidence{}, ErrCellularAccess
		}
	}
	return imsAccessEvidence{LocalIPv4: v4, LocalIPv6: v6,
		PCSCF: append([]string(nil), evidence.PCSCF...), PANI: pani}, nil
}

func cellularVerificationBudget(bearer vowifi.CellularIMSBearer) time.Duration {
	const fallback = 3 * time.Second
	if source, ok := bearer.(vowifi.CellularIMSVerificationBudget); ok {
		if budget := source.VerificationTimeout(); budget >= fallback && budget <= 25*time.Second {
			return budget
		}
	}
	return fallback
}

func usableBearerIP(ip net.IP) bool {
	// Loopback is allowed for the synthetic registrar/installer harness. Real
	// adapters must prove their exact physical interface and PDN before Verify.
	return ip != nil && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}

func (session *Session) verifyCellularAccess(ctx context.Context) error {
	if session.request.Cellular == nil {
		return nil
	}
	_, err := session.checkedCellularEvidence(ctx)
	return err
}

func (session *Session) checkedCellularEvidence(ctx context.Context) (imsAccessEvidence, error) {
	current, err := verifiedCellularEvidence(ctx, session.request)
	if err != nil {
		return imsAccessEvidence{}, err
	}
	if !sameBearerAddress(current.LocalIPv4, session.access.LocalIPv4) ||
		!sameBearerAddress(current.LocalIPv6, session.access.LocalIPv6) ||
		!pcscfProvenByTunnel(session.initialEndpoint, current.PCSCF, session.provider.config.Port) {
		return imsAccessEvidence{}, ErrCellularAccess
	}
	// A cellular UE may move without losing its PDN or registration. Carry the
	// newly verified access sample into subsequent INVITE/MESSAGE/dialog headers,
	// not only into REGISTER. Never update it from failed/unproven evidence.
	session.setPANI(current.PANI)
	return current, nil
}

func sessionPANI(request vowifi.IMSRequest, logger *slog.Logger) string {
	if request.Cellular != nil {
		// Assigned from verified cellular evidence before the first REGISTER.
		return ""
	}
	return resolveSessionPAccessNetworkInfo(request.Identity, logger)
}

func sameBearerAddress(left, right string) bool {
	return left == "" && right == "" || net.ParseIP(left) != nil && net.ParseIP(left).Equal(net.ParseIP(right))
}

func (provider *Provider) requestTransportCacheKey(request vowifi.IMSRequest) string {
	key := transportCacheKey(request.Identity)
	if request.Cellular != nil {
		// A successful Wi-Fi transport does not prove the cellular P-CSCF path.
		return "cellular:" + key
	}
	return key
}

func (provider *Provider) cachedRequestTransport(request vowifi.IMSRequest) string {
	provider.transportMu.RLock()
	defer provider.transportMu.RUnlock()
	return provider.transportCache[provider.requestTransportCacheKey(request)]
}

func (provider *Provider) rememberRequestTransport(request vowifi.IMSRequest, transport string) {
	provider.transportMu.Lock()
	defer provider.transportMu.Unlock()
	provider.transportCache[provider.requestTransportCacheKey(request)] = strings.ToLower(transport)
}
