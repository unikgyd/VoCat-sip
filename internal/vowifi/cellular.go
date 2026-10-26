package vowifi

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"time"

	"vocat/internal/vowifi/imssec"
)

// LTECell is observed serving-cell information, NOT the SIM's home PLMN.
// In particular a roaming SIM must not invent its current location from IMSI.
type LTECell struct {
	MCC    string
	MNC    string
	Duplex string // FDD or TDD
	TAC    uint16
	CellID uint32 // 28-bit E-UTRAN cell identity
}

func (cell LTECell) PAccessNetworkInfo() (string, error) {
	if !isNDigits(cell.MCC, 3, 3) || !isNDigits(cell.MNC, 2, 3) ||
		(cell.Duplex != "FDD" && cell.Duplex != "TDD") || cell.CellID > 0x0fffffff {
		return "", errors.New("IMS serving LTE cell information is invalid")
	}
	return fmt.Sprintf("3GPP-E-UTRAN-%s;utran-cell-id-3gpp=%s%s%04X%07X",
		cell.Duplex, cell.MCC, cell.MNC, cell.TAC, cell.CellID), nil
}

// CellularIMSEvidence is an owned, configured IMS PDN, not SWu/IKE evidence.
// The bearer owner must prove APN, hardware generation, selected SIM, local
// addresses, endpoint routes and P-CSCF provenance before reporting Established.
// P-CSCF entries are numeric addresses (with optional SIP port), never DNS.
type CellularIMSEvidence struct {
	Established bool
	LocalIPv4   string
	LocalIPv6   string
	PCSCF       []string
	Cell        LTECell
}

// CellularMediaRoute is an idempotent reference to one owned destination.
// Failed cleanup remains owned/fenced by the network owner, never forgotten.
type CellularMediaRoute interface {
	Close(context.Context) error
}

// CellularIMSNetwork is the data plane of the SAME owned bearer. BindSocket
// runs before connect/listen and verifies namespace, device and endpoint origin.
// Consumers must ALSO bind the exact assigned local IP. AcquireMediaRoute must
// finish before sending to a new SDP target; cancellation must independently
// observe and reclaim late kernel results. Neither method may start a PDN.
type CellularIMSNetwork interface {
	BindSocket(context.Context, string, string, syscall.RawConn) error
	AcquireMediaRoute(context.Context, netip.Addr, netip.Addr) (CellularMediaRoute, error)
}

// CellularIMSSecurity belongs to the SAME owner and generation as the bearer.
// Cellular IMS may never use an unrelated process-global ip-xfrm installer.
// BindProtectedSocket must verify the socket's namespace/device/local address,
// install required ESP IN/OUT policies with owner-unique reqids, and isolate
// overlapping modem addresses/SPIs (a device selector or if_id alone is NOT
// sufficient). It is called before listen/connect, and again for accepted TCP.
// The owner must reserve the socket protection scope before the first REGISTER.
// InstallIPSec must use that scope and a namespace-pinned XFRM transport; only
// acknowledged objects may be deleted. Errors/unknown ACKs remain fenced and
// owned there. Async owners clone keys before returning, then clear those copies
// when the bounded worker ends; never journal keys or raw add payloads. A returned
// handle remains caller-owned even if an error accompanies it.
// RetainIPSec transfers failed startup cleanup back to the long-lived owner:
// it must synchronously root the handle WITHOUT I/O, never reject it, and fence
// Verify and all new socket/security use of this bearer until independently
// acknowledged cleanup (without blocking another modem's independent owner).
// This is essential when Start returns an error and no session reaches callers.
type CellularIMSSecurity interface {
	BindProtectedSocket(context.Context, string, imssec.SocketBinding, syscall.RawConn) error
	InstallIPSec(context.Context, imssec.Config) (imssec.Handle, error)
	RetainIPSec(imssec.Handle)
}

// CellularIMSBearer does not transfer ownership to the SIP session. Its owner
// must stop IMS first, then remove only its own routes/addresses/PDN. Verify
// must be read-only, respect cancellation, reject SIM/hardware/route changes,
// and must NOT enable RF-off mode, start a PDN, restore module IMS or retry AKA.
// An AKAProvider supplied for Host IMS must independently pin the same card
// inside its authentication transaction (a preflight check alone has a TOCTOU).
type CellularIMSBearer interface {
	CellularIMSNetwork
	Evidence() CellularIMSEvidence
	Verify(context.Context, SIMIdentity) error
}

// CellularIMSVerificationBudget permits an owned QMI source to request a
// longer bounded refresh than a cheap cached adapter. Consumers clamp it and
// always preserve the caller's shorter deadline; it is not an I/O bypass.
type CellularIMSVerificationBudget interface {
	VerificationTimeout() time.Duration
}
