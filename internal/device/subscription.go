package device

import (
	"errors"
	"strings"
)

var ErrSIMObservation = errors.New("live SIM identity is not currently verified")

// Empty success means a positively observed empty slot, not a timed-out read
// or an inventory row that has not received its first snapshot yet.
func SnapshotSubscription(snapshot *Snapshot) (iccid, imsi string, err error) {
	if snapshot == nil {
		return "", "", ErrSIMObservation
	}
	iccid, imsi = strings.TrimSpace(snapshot.ICCID), strings.TrimSpace(snapshot.IMSI)
	if iccid != "" && imsi != "" {
		return iccid, imsi, nil
	}
	switch strings.ToLower(strings.TrimSpace(snapshot.SIMStatus)) {
	case "not_inserted", "not inserted", "absent":
		return "", "", nil
	default:
		return "", "", ErrSIMObservation
	}
}
