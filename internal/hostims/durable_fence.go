package hostims

import "context"

// HasEquipmentClaim is a presence-only cold-start fence, not recovery or a
// bearer capability. Even malformed/unknown records remain pending. It exposes
// neither the equipment-derived key nor the private WAL payload to callers.
func (j SQLiteClaimJournal) HasEquipmentClaim(ctx context.Context, equipment string) (bool, error) {
	if !asciiDigits(equipment, 15, 15) || equipment == "000000000000000" {
		return false, ErrSelection
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if j.Database == nil {
		return false, ErrJournal
	}
	present, err := j.Database.HasHostIMSClaim(ctx, "host.ims.claim.private.v1."+hashText("imei:"+equipment))
	if err != nil {
		return false, ErrJournal
	}
	return present, nil
}

// HasAnyClaim is only for a caller without a reliable equipment binding. It
// can prove the namespace empty, never infer which modem owns a present row.
func (j SQLiteClaimJournal) HasAnyClaim(ctx context.Context) (bool, error) {
	if j.Database == nil {
		return false, ErrJournal
	}
	if ctx == nil {
		ctx = context.Background()
	}
	present, err := j.Database.HasAnyHostIMSClaim(ctx)
	if err != nil {
		return false, ErrJournal
	}
	return present, nil
}
