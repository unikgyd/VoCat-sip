package vowifi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

const maxIMSGatewayAttempts = 16

type gatewayConnection struct {
	identity      SIMIdentity
	candidate     EPDGCandidate
	ims           IMSSession
	evidence      IMSEvidence
	cleanupErrors []string
}

// Keep the original radio checkpoint and verified subscriber while rebuilding
// only network layers. Authentication success pins the home: an IMS timeout
// must not become a reason to cycle MNCs or consume unrelated identities.
func (o *Orchestrator) connectGateways(ctx, runtime context.Context, resources *runtimeResources, attempts []EPDGAttempt, discovery EPDGDiscoveryOptions, card IKERetryCard) (gatewayConnection, error) {
	fail := func(stage Phase, err error, cleanup ...string) (gatewayConnection, error) {
		return gatewayConnection{cleanupErrors: cleanup}, &StageError{Stage: stage, Err: err}
	}
	proxies := map[string]ProxyRoute{}
	var pinnedHome string
	var lastIMS, lastTunnel error
	imsAttempts := 0
	lastIdentity := ""
candidateLoop:
	for index, attempt := range attempts {
		id, candidate := attempt.Identity, attempt.Candidate
		home := id.HomeMCC + "/" + id.HomeMNC
		identityHome := id.ICCID + "\n" + id.IMSI + "\n" + home
		if pinnedHome != "" && pinnedHome != identityHome {
			continue
		}
		if err := ctx.Err(); err != nil {
			return fail(PhaseTunnelReady, err)
		}
		if err := o.ikeRetry.Permit(ctx, card, o.ikeRetryTarget(candidate), ""); err != nil {
			if errors.Is(err, ErrIKERetryBlocked) {
				lastTunnel = err
				continue candidateLoop
			}
			return fail(PhaseTunnelReady, err)
		}
		if lastIdentity != "" && lastIdentity != id.IMSI {
			physical, err := AKAIdentity(id)
			if err != nil {
				return fail(PhaseSIMReady, err)
			}
			ready, err := o.deps.AKA.CheckReady(ctx, physical)
			if err != nil {
				return fail(PhaseSIMReady, err)
			}
			if !ready.Ready {
				return fail(PhaseSIMReady, errors.New("AKA application is not ready for the next profile identity"))
			}
		}
		lastIdentity = id.IMSI
		proxy, cached := proxies[home]
		if !cached {
			var err error
			proxy, err = o.deps.Proxy.Resolve(ctx, ProxyRequest{DeviceID: o.options.DeviceID, ICCID: id.ICCID, HomeMCC: id.HomeMCC, HomeMNC: id.HomeMNC, CountryCode: strings.ToUpper(strings.TrimSpace(id.HomeCountryCode))})
			if err != nil {
				return fail(PhaseAccessReady, err)
			}
			proxy, err = normalizeProxyRoute(proxy)
			if err != nil {
				return fail(PhaseAccessReady, err)
			}
			proxies[home] = proxy
		}
		var excluded []string
		for {
			if err := ctx.Err(); err != nil {
				return fail(PhaseIMSReady, err)
			}
			if imsAttempts >= maxIMSGatewayAttempts {
				return fail(PhaseIMSReady, lastIMS)
			}
			profile := ResolveCarrierProfile(id)
			o.mutate(func(s *State) {
				s.Phase = PhaseAccessReady
				s.AccessReady = true
				s.Active = false
				s.TunnelReady = false
				s.IMSReady = false
				s.SMSReady = false
				s.HomeMCC = id.HomeMCC
				s.HomeMNC = id.HomeMNC
				s.HomeMNCUncertain = id.HomeMNCUncertain
				s.CarrierProfile = profile.ID
				s.CarrierProfileFrom = profile.MatchSource
				s.ProxyMode = proxy.Mode
				s.ProxyID = proxy.ID
				s.EPDG = candidate.Host
				s.EPDGSource = candidate.Source
				s.EPDGAttempt = index + 1
				s.EPDGCandidates = len(attempts)
				s.EPDGAddress = ""
				s.TunnelName = ""
				s.DataplaneMode = ""
				s.IMSRegistration = ""
				s.Security = SecurityAudit{ResponderAUTH: ResponderAUTHUnknown}
				s.LastReason = "epdg_access_ready"
			})
			tunnel, err := o.startGovernedTunnel(ctx, card, candidate, TunnelRequest{ExcludedEPDGAddresses: append([]string(nil), excluded...), GeographicDNSFallback: discovery.GeographicDNSFallback, DeviceID: o.options.DeviceID, Identity: id, EPDG: candidate.Host, Proxy: proxy, AKA: o.deps.AKA, Security: TunnelSecurityPolicy{AllowMissingResponderAUTH: o.options.AllowMissingResponderAUTH}})
			resources.tunnel = tunnel
			if err != nil {
				lastTunnel = err
				canContinue := ctx.Err() == nil && (EPDGFallbackAllowed(err) || (len(excluded) > 0 && everyErrorCause(err, func(cause error) bool { return cause == ErrEPDGAddressesExhausted })))
				if !canContinue {
					return fail(PhaseTunnelReady, err)
				}
				if cleanup := o.cleanupSessions(resources); len(cleanup) > 0 {
					return fail(PhaseTunnelReady, errors.Join(err, ErrCleanupIncomplete), cleanup...)
				}
				continue candidateLoop
			}
			if tunnel == nil {
				return fail(PhaseTunnelReady, errors.New("tunnel provider returned a nil session"))
			}
			evidence := tunnel.Evidence()
			o.mutate(func(s *State) { s.EPDGAddress = evidence.EPDGAddress })
			if !evidence.Established {
				o.mutate(func(s *State) { s.Security = securityAuditFromEvidence(evidence) })
				return fail(PhaseTunnelReady, ErrTunnelNotEstablished)
			}
			audit, err := o.validateTunnelEvidence(evidence)
			o.mutate(func(s *State) { s.Security = audit })
			if err != nil {
				return fail(PhaseTunnelReady, err)
			}
			ip := net.ParseIP(evidence.EPDGAddress)
			for _, previous := range excluded {
				if ip != nil && ip.Equal(net.ParseIP(previous)) {
					return fail(PhaseTunnelReady, errors.New("tunnel provider reused an excluded ePDG address"))
				}
			}
			pinnedHome = identityHome
			o.mutate(func(s *State) {
				s.Phase = PhaseTunnelReady
				s.Active = true
				s.TunnelReady = true
				s.TunnelName = strings.TrimSpace(evidence.Name)
				s.DataplaneMode = strings.TrimSpace(evidence.DataplaneMode)
				s.LastReason = "ipsec_tunnel_ready"
			})
			watchCtx, stopWatch := context.WithCancel(runtime)
			o.mu.Lock()
			resources.watchCancel = stopWatch
			o.mu.Unlock()
			o.watchRuntimeTunnel(watchCtx, resources, tunnel)
			ims, err := o.deps.IMS.Start(ctx, IMSRequest{DeviceID: o.options.DeviceID, Identity: id, Tunnel: tunnel})
			imsAttempts++
			resources.ims = ims
			if err != nil {
				if ims != nil {
					partial := ims.Evidence()
					if partial.Registered || partial.LastSIPCode != 0 {
						return fail(PhaseIMSReady, err)
					}
				}
				if ctx.Err() != nil || !IMSFallbackAllowed(err) {
					return fail(PhaseIMSReady, err)
				}
				if !UsableEPDGAddress(ip) {
					return fail(PhaseIMSReady, fmt.Errorf("IMS transport failed without a proven ePDG IP: %w", err))
				}
				if cleanup := o.cleanupSessions(resources); len(cleanup) > 0 {
					return fail(PhaseIMSReady, errors.Join(err, ErrCleanupIncomplete), cleanup...)
				}
				lastIMS = err
				excluded = append(excluded, ip.String())
				continue
			}
			if ims == nil {
				return fail(PhaseIMSReady, errors.New("IMS provider returned a nil session"))
			}
			registered := ims.Evidence()
			if !registered.Registered {
				return fail(PhaseIMSReady, ErrIMSNotRegistered)
			}
			o.watchRuntimeIMS(watchCtx, resources, ims)
			return gatewayConnection{identity: id, candidate: candidate, ims: ims, evidence: registered}, nil
		}
	}
	if lastIMS != nil {
		return fail(PhaseIMSReady, lastIMS)
	}
	if lastTunnel == nil {
		lastTunnel = errors.New("no ePDG gateway completed setup")
	}
	return fail(PhaseTunnelReady, lastTunnel)
}
