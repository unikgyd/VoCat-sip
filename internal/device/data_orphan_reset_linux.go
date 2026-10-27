//go:build linux

package device

import (
	"context"
	"errors"
	"io"
	"time"

	"vocat/internal/modem"
)

// OrphanDataRecovery deliberately exposes neither a WDS client nor a handle.
// Its implementation holds the original equipment lock and durable record.
type OrphanDataRecovery interface {
	VerifyIdentity(modem.Candidate, string, string, string) error
	BeginRecovery(context.Context, modem.Candidate, string, string, string) (bool, error)
}

// RestartOrphanData uses only an explicitly configured secondary AT port.
// The returned serial capability must be retained until the original USB
// generation disappears, including after an interrupted query/reset reply.
func RestartOrphanData(ctx context.Context, candidate modem.Candidate, configuredPort string, original OrphanDataRecovery, allow func(context.Context, modem.Candidate, string) bool) (io.Closer, bool, error) {
	return restartOrphanData(ctx, candidate, configuredPort, original, allow, modem.SerialOpener{}, verifyDataRecoveryPort)
}

func restartOrphanData(ctx context.Context, candidate modem.Candidate, configuredPort string, original OrphanDataRecovery, allow func(context.Context, modem.Candidate, string) bool, opener modem.Opener, verifyPort func(modem.Candidate, modem.Port) error) (retained io.Closer, submitted bool, resultErr error) {
	if original == nil || allow == nil || opener == nil || ctx.Err() != nil {
		return nil, false, ErrDataCleanupPending
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	port, err := dataRecoveryPort(candidate, configuredPort)
	if err != nil || verifyPort(candidate, port) != nil {
		return nil, false, ErrSIMObservation
	}
	opened, err := opener.Open(ctx, port)
	if err != nil {
		return nil, false, err
	}
	client := &dataRecoveryAT{Client: opened}
	defer func() {
		if retained == nil {
			if client.uncertain {
				retained = client
			} else if err := client.Close(); err != nil {
				retained, resultErr = client, errors.Join(resultErr, ErrDataCleanupPending)
			}
		}
	}()
	verify := func() (string, string, string, error) {
		reader := atSIMWriteReader{client}
		iccid, err := reader.GetICCID(ctx)
		if err != nil {
			return "", "", "", err
		}
		imsi, err := reader.GetIMSI(ctx)
		if err != nil {
			return "", "", "", err
		}
		response, err := client.Execute(ctx, "AT+CGSN")
		if err != nil || !response.OK() {
			return "", "", "", ErrSIMObservation
		}
		equipment := parseIdentifier(response, []string{"+CGSN:", "+GSN:"}, 15, 15)
		againICCID, err := reader.GetICCID(ctx)
		if err != nil || againICCID != iccid {
			return "", "", "", ErrSIMObservation
		}
		againIMSI, err := reader.GetIMSI(ctx)
		if err != nil || againIMSI != imsi {
			return "", "", "", ErrSIMObservation
		}
		if err := original.VerifyIdentity(candidate, equipment, iccid, imsi); err != nil {
			return "", "", "", err
		}
		if err := (&Manager{}).recoveryRestartPreflight(ctx, client); err != nil {
			return "", "", "", err
		}
		if ctx.Err() != nil || verifyPort(candidate, port) != nil || !allow(ctx, candidate, iccid) {
			return "", "", "", ErrDataCleanupPending
		}
		return equipment, iccid, imsi, nil
	}
	equipment, iccid, imsi, err := verify()
	if err != nil {
		return nil, false, err
	}
	admitted, err := original.BeginRecovery(ctx, candidate, equipment, iccid, imsi)
	if err != nil || !admitted {
		return nil, false, errors.Join(ErrDataCleanupPending, err)
	}
	// Repeat all live guards after the synchronous intent commit. No failure
	// below may replay the intent on a later tick or replacement owner.
	if _, _, _, err := verify(); err != nil {
		return nil, false, err
	}
	retained = client
	_, _ = client.Execute(ctx, "AT+CFUN=1,1")
	return retained, true, ErrDataCleanupPending
}
