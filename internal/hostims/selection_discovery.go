package hostims

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
)

// ModemTarget is an expected live inventory identity, never a request to pick
// the first modem. Callers must resolve Physical from trusted sysfs discovery.
// Do not log this value: it contains subscriber and equipment identities.
type ModemTarget struct{ Equipment, Physical, QMIControl, ICCID, IMSI string }

func (ModemTarget) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private expected cellular target>")
}

func validModemTarget(t ModemTarget) bool {
	return asciiDigits(t.Equipment, 15, 15) && strings.Trim(t.Equipment, "0") != "" &&
		(t.ICCID == "" && t.IMSI == "" || asciiDigits(t.ICCID, 18, 20) && asciiDigits(t.IMSI, 14, 16)) &&
		strings.HasPrefix(t.Physical, "/sys/devices/") && path.Clean(t.Physical) == t.Physical &&
		strings.HasPrefix(t.QMIControl, "/dev/") && path.Dir(t.QMIControl) == "/dev" && path.Base(t.QMIControl) != "."
}

func samePhysicalTree(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

// SelectTarget enumerates all MM equipment identities before selecting exactly
// one. The normal Select/verify path then pins service, physical generation,
// SIM/slot and bearer ownership; the target additionally binds app discovery.
func (p *BearerPool) SelectTarget(ctx context.Context, target ModemTarget) (Selection, error) {
	if p == nil || !validModemTarget(target) {
		return Selection{}, ErrSelection
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Selection{}, err
	}
	paths, err := p.backend.Modems(ctx)
	if err != nil || len(paths) == 0 || len(paths) > 64 {
		return Selection{}, ErrSelection
	}
	match := ""
	for candidate, equipment := range paths {
		if !objectPath(candidate, "Modem") {
			return Selection{}, ErrSelection
		}
		if equipment != target.Equipment {
			continue
		}
		if match != "" {
			return Selection{}, ErrSelection
		}
		match = candidate
	}
	if match == "" {
		return Selection{}, ErrSelection
	}
	selection, err := p.Select(ctx, match)
	if err != nil {
		return Selection{}, err
	}
	if err := ctx.Err(); err != nil {
		return Selection{}, err
	}
	b := selection.snapshot.Binding
	if b.Equipment != target.Equipment || target.ICCID != "" && b.ICCID != target.ICCID || target.IMSI != "" && b.IMSI != target.IMSI ||
		selection.snapshot.QMIControl != target.QMIControl || !samePhysicalTree(b.Physical, target.Physical) {
		return Selection{}, ErrChanged
	}
	return selection, nil
}

// HasPendingClaims protects application-owned transports from shutdown while
// any late/uncertain owner can still need them. It grants no cleanup authority.
func (p *BearerPool) HasPendingClaims() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	claims := make([]*bearerClaim, 0, len(p.claims))
	for _, claim := range p.claims {
		claims = append(claims, claim)
	}
	p.mu.Unlock()
	for _, claim := range claims {
		claim.mu.Lock()
		pending := pendingClaimLocked(claim)
		claim.mu.Unlock()
		if pending {
			return true
		}
	}
	return false
}
