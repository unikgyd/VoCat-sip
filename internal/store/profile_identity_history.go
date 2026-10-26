package store

import (
	"context"
	"encoding/json"
)

const ProfileIdentityHistoryKey = "vowifi.sim.identities.private.v1"
const MaxProfileIdentityHistoryBytes = 262144

// Own namespace, exact bytes, cross-process CAS and the same confirmed durable
// commit as other private journals. History never grants a billing permission.
func (s *Store) CompareProfileIdentityHistory(ctx context.Context, before, after json.RawMessage) (bool, error) {
	return s.compareSIPPrivateState(ctx, ProfileIdentityHistoryKey, before, after, MaxProfileIdentityHistoryBytes)
}
