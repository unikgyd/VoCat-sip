package hostims

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"vocat/internal/vowifi"
)

// ErrServingCellUnavailable deliberately does not include location, subscriber
// identifiers or untrusted D-Bus error text.
var ErrServingCellUnavailable = staticError("host IMS: fresh LTE serving cell and duplex are unavailable")

// CachedServingCell is diagnostic data, NOT a fresh LTECell or PANI proof.
// ModemManager Location.GetLocation returns its cached 3GPP location without a
// sample timestamp, cell generation, or LTE FDD/TDD. Repeated equal reads cannot
// establish freshness or rule out an unobserved A -> B -> A cell transition.
// In particular, the time of this API call must not be used as the sample time.
type CachedServingCell struct {
	mcc, mnc string
	tac      uint16
	eci      uint32
}

func (CachedServingCell) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private cached serving cell; age and duplex unknown>")
}

// PLMN preserves the observed MNC's two- or three-digit width. It is never
// derived from the selected SIM's IMSI or home operator.
func (c CachedServingCell) PLMN() (mcc, mnc string)  { return c.mcc, c.mnc }
func (c CachedServingCell) TrackingAreaCode() uint16 { return c.tac }
func (c CachedServingCell) EUTRANCellID() uint32     { return c.eci }

// PAccessNetworkInfo cannot promote a cached location into cellular evidence.
func (CachedServingCell) PAccessNetworkInfo() (string, error) {
	return "", ErrServingCellUnavailable
}

type servingCellSnapshot struct {
	cell                  CachedServingCell
	operator              string
	state                 int32
	access, registration  uint32
	capabilities, enabled uint32
}

type servingCellReader func(context.Context, string, string) (servingCellSnapshot, error)

// ServingCellSource is a bounded, read-only observer tied to an owned bearer.
// It has no API for enabling location, changing radio state or starting IMS.
// It does not implement CellularIMSBearer or retain a reusable cell proof.
type ServingCellSource struct {
	lease *BearerLease
	read  servingCellReader
}

func (ServingCellSource) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private owned serving cell source>")
}

func newServingCellSource(lease *BearerLease, read servingCellReader) (*ServingCellSource, error) {
	if lease == nil || lease.pool == nil || lease.claim == nil || lease.selection.pool != lease.pool || read == nil {
		return nil, ErrSelection
	}
	return &ServingCellSource{lease: lease, read: read}, nil
}

func (s *ServingCellSource) verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.lease.pool.authorize(ctx, s.lease.selection) != nil {
		return ErrDenied
	}
	return s.lease.Verify(ctx)
}

// ObserveCached returns only consistent diagnostic snapshots. All calls pin
// the selected service owner, hardware generation, SIM and owned connected PDN
// before, between and after the location reads. A disabled source, non-LTE RAT,
// unavailable TAC/ECI, or observed generation transition yields no observation.
// Equal snapshots still have unknown age and are explicitly NOT fresh proof.
func (s *ServingCellSource) ObserveCached(ctx context.Context) (CachedServingCell, error) {
	if s == nil || s.lease == nil || s.read == nil {
		return CachedServingCell{}, ErrSelection
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var first servingCellSnapshot
	for index := 0; index < 2; index++ {
		if err := s.verify(ctx); err != nil {
			return CachedServingCell{}, safeServingCellError(ctx, err)
		}
		current, err := s.read(ctx, s.lease.selection.snapshot.Owner, s.lease.selection.snapshot.Path)
		if err != nil {
			return CachedServingCell{}, safeServingCellError(ctx, err)
		}
		if index == 0 {
			first = current
		} else if current != first {
			return CachedServingCell{}, ErrChanged
		}
	}
	if err := s.verify(ctx); err != nil {
		return CachedServingCell{}, safeServingCellError(ctx, err)
	}
	return first.cell, nil
}

// Read is the fail-closed LTE/PANI source boundary for a future Host IMS owner.
// The current MM Location interface cannot supply fresh sample provenance and
// duplex, so even a consistent diagnostic observation NEVER becomes LTECell.
// A future live radio query must independently pin the same serving-cell
// generation and provide both missing facts before this boundary may succeed.
func (s *ServingCellSource) Read(ctx context.Context) (vowifi.LTECell, error) {
	if _, err := s.ObserveCached(ctx); err != nil {
		return vowifi.LTECell{}, err
	}
	return vowifi.LTECell{}, ErrServingCellUnavailable
}

func safeServingCellError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, known := range []error{ErrServingCellUnavailable, ErrChanged, ErrSelection, ErrDenied, ErrBusy, ErrUncertain, ErrJournal, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrServingCellUnavailable
}

func parseCachedLTELocation(raw, operator string) (CachedServingCell, error) {
	if len(raw) > 64 {
		return CachedServingCell{}, ErrServingCellUnavailable
	}
	parts := strings.Split(raw, ",")
	// Four-field pre-1.10 responses have LAC, not an LTE TAC. Never substitute it.
	if len(parts) != 5 || !asciiDigits(parts[0], 3, 3) || !asciiDigits(parts[1], 2, 3) || parts[0]+parts[1] != operator {
		return CachedServingCell{}, ErrServingCellUnavailable
	}
	hex := func(value string, digits, bits int) (uint64, bool) {
		if value == "" || len(value) > digits {
			return 0, false
		}
		for _, char := range value {
			if !(char >= '0' && char <= '9' || char >= 'A' && char <= 'F') {
				return 0, false
			}
		}
		parsed, err := strconv.ParseUint(value, 16, bits)
		return parsed, err == nil
	}
	_, lacOK := hex(parts[2], 4, 16)
	eci, eciOK := hex(parts[3], 7, 28)
	tac, tacOK := hex(parts[4], 4, 16)
	// MM uses zero for an unknown CI/area. LTE TAC is 16-bit; do not truncate
	// a 24-bit 5GNR TAC or a 32/36-bit cell identity to make an LTE PANI fit.
	if !lacOK || !eciOK || !tacOK || eci == 0 || tac == 0 {
		return CachedServingCell{}, ErrServingCellUnavailable
	}
	return CachedServingCell{mcc: parts[0], mnc: parts[1], tac: uint16(tac), eci: uint32(eci)}, nil
}
