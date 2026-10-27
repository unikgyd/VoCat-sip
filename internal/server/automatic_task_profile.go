package server

import (
	"context"
	"errors"
	"strings"

	"vocat/internal/device"
)

type automaticTaskProfileStorage struct {
	AID string
	EID string
}

// Profile AIDs address ISD-P applications inside EnableProfile. The transport
// must instead select the containing storage's ISD-R management application.
// Resolve that storage from live inventory, including when an older journal
// incorrectly retained the profile AID as its management AID.
func (s *Server) taskProfileStorage(ctx context.Context, physical, iccid, expectedEID string) (automaticTaskProfileStorage, error) {
	entries, err := s.devices.ESIMInventory(ctx, physical)
	if err != nil {
		return automaticTaskProfileStorage{}, err
	}
	var found automaticTaskProfileStorage
	for _, entry := range entries {
		contains := false
		for _, profile := range entry.Info.Profiles {
			if strings.TrimSpace(profile.ICCID) == iccid {
				contains = true
				break
			}
		}
		if !contains {
			continue
		}
		eid, chipEID := strings.TrimSpace(entry.Info.EID), strings.TrimSpace(entry.Chip.EID)
		aid, chipAID := strings.ToUpper(strings.TrimSpace(entry.Info.AID)), strings.ToUpper(strings.TrimSpace(entry.Chip.AID))
		if eid != "" && chipEID != "" && eid != chipEID || aid != "" && chipAID != "" && aid != chipAID {
			return automaticTaskProfileStorage{}, device.ErrSIMObservation
		}
		eid, aid = firstNonEmpty(eid, chipEID), firstNonEmpty(aid, chipAID)
		if expectedEID != "" && eid != expectedEID {
			continue
		}
		if eid == "" || aid == "" || found.EID != "" {
			return automaticTaskProfileStorage{}, device.ErrSIMObservation
		}
		found = automaticTaskProfileStorage{AID: aid, EID: eid}
	}
	if found.EID == "" {
		return automaticTaskProfileStorage{}, errors.Join(device.ErrSIMObservation, errors.New("original profile storage must be verified before an automatic profile switch"))
	}
	return found, nil
}
