package store

import (
	"context"
	"encoding/json"
)

const IKERetryJournalKey = "vowifi.ike.retry.private.v1"
const MaxIKERetryJournalBytes = 1 << 20

// Dedicated private namespace, exact CAS and confirmed FULL-synchronous commit.
// This is not the best-effort ePDG success-history cache.
func (s *Store) CompareIKERetryJournal(ctx context.Context, before, after json.RawMessage) (bool, error) {
	return s.compareSIPPrivateState(ctx, IKERetryJournalKey, before, after, MaxIKERetryJournalBytes)
}
