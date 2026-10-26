package sipgateway

import (
	"context"

	"vocat/internal/store"
)

// Preserve the original awaited-authority/source-veto test bodies verbatim.
// Its old AppSetting hook pauses before the real coherent snapshot is read;
// the discarded preliminary value never supplies wire authorization.
func (database *smsCaptureWireAuthorityPause) ReadSMSForwardWireSnapshot(ctx context.Context, guard *store.SMSForwardInboxGuard) (store.SMSForwardWireSnapshot, error) {
	if _, err := database.AppSetting(ctx, SettingKey); err != nil {
		return store.SMSForwardWireSnapshot{}, err
	}
	return database.Store.ReadSMSForwardWireSnapshot(ctx, guard)
}
