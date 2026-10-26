package hostims

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"vocat/internal/store"
)

const maxClaimBytes = store.MaxHostIMSClaimBytes

// ClaimJournal must implement atomic insert/compare-update/delete across all
// pools/processes sharing it. Claims never expire: a crash is not a release.
type ClaimJournal interface {
	Lock(string) (io.Closer, error)
	Read(context.Context, string) ([]byte, error)
	Compare(context.Context, string, []byte, []byte) (bool, error)
}
type SQLiteClaimJournal struct {
	Database      *store.Store
	lockDirectory string
}

func (j SQLiteClaimJournal) Lock(key string) (io.Closer, error) {
	directory := j.lockDirectory
	if directory == "" {
		directory = "/run/lock/vocat-host-ims"
	}
	return lockClaimAt(directory, key)
}
func (j SQLiteClaimJournal) Read(ctx context.Context, key string) ([]byte, error) {
	if j.Database == nil {
		return nil, ErrJournal
	}
	value, err := j.Database.AppSetting(ctx, "host.ims.claim.private.v1."+key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil || !value.Sensitive {
		return nil, ErrJournal
	}
	return append([]byte(nil), value.Value...), nil
}
func (j SQLiteClaimJournal) Compare(ctx context.Context, key string, before, after []byte) (bool, error) {
	if j.Database == nil {
		return false, ErrJournal
	}
	return j.Database.CompareHostIMSClaim(ctx, "host.ims.claim.private.v1."+key, json.RawMessage(before), json.RawMessage(after))
}
