package imssec

import (
	"errors"
	"net/netip"
	"strings"
)

// State and Policy contain scalar, detached selectors only. Keys/transforms
// remain in Config for the installer, not in a reusable/serializable plan.
type State struct {
	Description string
	Src, Dst    netip.Addr
	SPI, ReqID  uint32
}

type Policy struct {
	Description              string
	Src, Dst                 netip.Prefix
	Protocol                 uint8  // TCP=6 or UDP=17
	SrcPort, DstPort         uint16 // zero means a wildcard, not a transport port
	Direction                string // in or out, relative to the UE
	TemplateSrc, TemplateDst netip.Addr
	SPI, ReqID               uint32
}

// Plan always has four transport-mode ESP states and all eight TCP/UDP
// policies, in installation order. Cleanup must invert this exact order and
// act only on objects whose addition was independently acknowledged.
type Plan struct {
	States   [4]State
	Policies [8]Policy
}

func BuildPlan(config Config) (Plan, error) {
	return BuildScopedPlan(config, ClientPairReqID(config), ServerPairReqID(config))
}

// BuildScopedPlan binds the canonical agreement to owner-reserved reqids. The
// owner must reserve both before exposing protected sockets, and use exactly
// these reqids for socket policies and states. A plan still proves no ownership
// of existing kernel objects and supplies no state deletion key or if_id.
func BuildScopedPlan(config Config, clientReqID, serverReqID uint32) (Plan, error) {
	if err := ValidateConfig(config); err != nil {
		return Plan{}, err
	}
	if clientReqID == 0 || serverReqID == 0 || clientReqID == serverReqID {
		return Plan{}, errors.New("ims: ipsec-3gpp scoped reqids must be nonzero and distinct")
	}
	local, _ := netip.AddrFromSlice(config.LocalIP)
	remote, _ := netip.AddrFromSlice(config.RemoteIP)
	local, remote = local.Unmap(), remote.Unmap()
	localPrefix, remotePrefix := netip.PrefixFrom(local, local.BitLen()), netip.PrefixFrom(remote, remote.BitLen())
	client, server := clientReqID, serverReqID
	plan := Plan{States: [4]State{
		{"outbound UE-client to P-CSCF-server state", local, remote, config.PCSCFServerSPI, client},
		{"inbound P-CSCF-server to UE-client state", remote, local, config.UEClientSPI, client},
		{"inbound P-CSCF-client to UE-server state", remote, local, config.UEServerSPI, server},
		{"outbound UE-server to P-CSCF-client state", local, remote, config.PCSCFClientSPI, server},
	}}
	flows := [4]Policy{
		{Description: "UE-client to P-CSCF-server", Src: localPrefix, Dst: remotePrefix,
			SrcPort: uint16(config.UEClientPort), DstPort: uint16(config.PCSCFServerPort), Direction: "out",
			TemplateSrc: local, TemplateDst: remote, SPI: config.PCSCFServerSPI, ReqID: client},
		{Description: "P-CSCF-server to UE-client", Src: remotePrefix, Dst: localPrefix,
			SrcPort: uint16(config.PCSCFServerPort), DstPort: uint16(config.UEClientPort), Direction: "in",
			TemplateSrc: remote, TemplateDst: local, SPI: config.UEClientSPI, ReqID: client},
		{Description: "P-CSCF-client to UE-server", Src: remotePrefix, Dst: localPrefix,
			SrcPort: 0, DstPort: uint16(config.UEServerPort), Direction: "in",
			TemplateSrc: remote, TemplateDst: local, SPI: config.UEServerSPI, ReqID: server},
		// A proven P-CSCF may initiate protected TCP or UDP from an ephemeral
		// port. Only the server pair has peer-port wildcards; addresses and
		// the UE server port remain exact in both directions.
		{Description: "UE-server to P-CSCF-client", Src: localPrefix, Dst: remotePrefix,
			SrcPort: uint16(config.UEServerPort), DstPort: 0, Direction: "out",
			TemplateSrc: local, TemplateDst: remote, SPI: config.PCSCFClientSPI, ReqID: server},
	}
	for index, flow := range flows {
		for protocolIndex, protocol := range [...]uint8{6, 17} {
			flow.Protocol = protocol
			plan.Policies[index*2+protocolIndex] = flow
		}
	}
	return plan, nil
}

// ValidatePlan refuses altered/reordered/extra selectors, changed SPI/reqid
// bindings and missing protocols instead of accepting a merely similar plan.
func ValidatePlan(config Config, plan Plan) error {
	return ValidateScopedPlan(config, ClientPairReqID(config), ServerPairReqID(config), plan)
}

func ValidateScopedPlan(config Config, clientReqID, serverReqID uint32, plan Plan) error {
	expected, err := BuildScopedPlan(config, clientReqID, serverReqID)
	if err != nil {
		return err
	}
	if plan != expected {
		return errors.New("ims: ipsec-3gpp plan does not match the agreement")
	}
	return nil
}

func ValidProtectedPort(port int) bool {
	return port > 1024 && port <= 65535 && port != 5060 && port != 5061
}

func ValidateConfig(config Config) error {
	local, localOK := netip.AddrFromSlice(config.LocalIP)
	remote, remoteOK := netip.AddrFromSlice(config.RemoteIP)
	local, remote = local.Unmap(), remote.Unmap()
	if !localOK || !remoteOK || local.IsUnspecified() || remote.IsUnspecified() || local.Is4() != remote.Is4() {
		return errors.New("ims: ipsec-3gpp endpoints are invalid or use different IP families")
	}
	spis := [4]uint32{config.UEClientSPI, config.UEServerSPI, config.PCSCFClientSPI, config.PCSCFServerSPI}
	seen := make(map[uint32]bool, len(spis))
	for _, spi := range spis {
		if spi == 0 {
			return errors.New("ims: ipsec-3gpp SPI is zero")
		}
		if seen[spi] {
			return errors.New("ims: ipsec-3gpp SPIs must be unique")
		}
		seen[spi] = true
	}
	for _, port := range [4]int{config.UEClientPort, config.UEServerPort, config.PCSCFClientPort, config.PCSCFServerPort} {
		if !ValidProtectedPort(port) {
			return errors.New("ims: ipsec-3gpp protected port is invalid")
		}
	}
	if config.UEClientPort == config.UEServerPort || config.PCSCFClientPort == config.PCSCFServerPort {
		return errors.New("ims: client and server protected ports must differ")
	}
	encryption, integrity := EncryptionAlgorithm(config), IntegrityAlgorithm(config)
	encryptionLengths := map[string]int{"null": 0, "aes-cbc": 16, "des-ede3-cbc": 24}
	integrityLengths := map[string]int{"hmac-md5-96": 16, "hmac-sha-1-96": 20}
	encryptionLength, encryptionOK := encryptionLengths[encryption]
	integrityLength, integrityOK := integrityLengths[integrity]
	if !encryptionOK {
		return errors.New("ims: unsupported ipsec-3gpp encryption algorithm")
	}
	if !integrityOK {
		return errors.New("ims: unsupported ipsec-3gpp integrity algorithm")
	}
	if len(config.EncryptionKey) != encryptionLength || len(config.IntegrityKey) != integrityLength {
		return errors.New("ims: ipsec-3gpp key length is invalid")
	}
	return nil
}

func IntegrityAlgorithm(config Config) string {
	algorithm := strings.ToLower(strings.TrimSpace(config.IntegrityAlgorithm))
	if algorithm == "" {
		return "hmac-sha-1-96"
	}
	return algorithm
}

func EncryptionAlgorithm(config Config) string {
	algorithm := strings.ToLower(strings.TrimSpace(config.EncryptionAlgorithm))
	if algorithm == "" {
		return "aes-cbc"
	}
	return algorithm
}

func ClientPairReqID(config Config) uint32 {
	reqid := (config.UEClientSPI ^ config.PCSCFServerSPI) & 0x7fffffff
	if reqid == 0 {
		return 1
	}
	return reqid
}

func ServerPairReqID(config Config) uint32 {
	reqid := (config.UEServerSPI ^ config.PCSCFClientSPI) & 0x7fffffff
	if reqid == 0 {
		reqid = 2
	}
	if reqid == ClientPairReqID(config) {
		reqid ^= 0x40000000
		if reqid == 0 {
			reqid = 2
		}
	}
	return reqid
}
