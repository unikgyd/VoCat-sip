package sipgateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// Epochs are private recipient identities, not caller-controlled credentials.
// In particular, hashing an unchanged password cannot identify a deleted and
// recreated account or an explicit same-password reset.
func validLocalCredentialEpoch(epoch string) bool {
	if len(epoch) != 32 {
		return false
	}
	for _, c := range epoch {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func newConfigRevision() (string, error) {
	var revision [16]byte
	if _, err := rand.Read(revision[:]); err != nil {
		return "", errors.New("SIP settings revision could not be generated")
	}
	return hex.EncodeToString(revision[:]), nil
}

func hasLocalAccount(config Config, user string) bool {
	for _, account := range config.Accounts {
		if account.Username == user {
			return true
		}
	}
	return false
}

func prepareLocalCredentialEpoch(realm string, account Account, credentials secret, old stored) (string, error) {
	epoch := old.LocalAccountEpochs[account.Username]
	if realm == old.Config.Realm && account.Password == "" && !account.ResetCredentialIdentity &&
		hasLocalAccount(old.Config, account.Username) && old.Secrets[account.Username] == credentials && validLocalCredentialEpoch(epoch) {
		return epoch, nil
	}
	var fresh [16]byte
	if _, err := rand.Read(fresh[:]); err != nil {
		return "", errors.New("SIP recipient identity could not be generated")
	}
	return hex.EncodeToString(fresh[:]), nil
}

// Missing or malformed legacy epochs must never fall back to a password-derived
// identity. Manager.New persists the migration before listener admission.
func localRecipientIdentity(config stored, user string) string {
	epoch := config.LocalAccountEpochs[user]
	credentials, exists := config.Secrets[user]
	if !exists || !hasLocalAccount(config.Config, user) || !validLocalCredentialEpoch(epoch) || credentials.MD5 == "" || credentials.SHA256 == "" {
		return ""
	}
	digest := sha256.New()
	for _, value := range []string{"vocat-local-recipient-v1", config.Config.Realm, user, epoch, credentials.MD5, credentials.SHA256} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = digest.Write(size[:])
		_, _ = digest.Write([]byte(value))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// A process-local Manager mutex cannot fence another process's account reset or
// policy change. Paid admission and MT claims use this fresh durable snapshot,
// then compare their original recipient/line authority and current permissions.
// This never generates or repairs an epoch: migration belongs to Manager.New.
func readCurrentConfigAuthority(ctx context.Context, db repository, admitted stored) (stored, error) {
	if !admitted.requireDurableAuthority {
		return admitted, nil // Direct synthetic runtime, not a durable Manager.
	}
	value, err := db.AppSetting(ctx, SettingKey)
	if err != nil || !value.Sensitive || len(value.Value) == 0 || len(value.Value) > 1<<20 {
		return stored{}, errors.New("current durable SIP authority is unavailable")
	}
	var current stored
	if json.Unmarshal(value.Value, &current) != nil || current.Version != 1 || !validLocalCredentialEpoch(current.ConfigRevision) || !realmPattern.MatchString(current.Config.Realm) ||
		len(current.Config.Accounts) > 64 || len(current.LocalAccountEpochs) != len(current.Config.Accounts) {
		return stored{}, errors.New("current durable SIP authority is invalid")
	}
	seen := map[string]bool{}
	for _, account := range current.Config.Accounts {
		if !extensionPattern.MatchString(account.Username) || seen[account.Username] || account.Password != "" || account.ResetCredentialIdentity ||
			!validLocalCredentialEpoch(current.LocalAccountEpochs[account.Username]) || !validPeerSecret(current.Secrets[account.Username]) {
			return stored{}, errors.New("current durable SIP recipient identity is invalid")
		}
		seen[account.Username] = true
	}
	if peer := current.Config.Peer; peer != nil {
		configured := peer.Enabled || peer.Endpoint != "" || peer.InboundAuth.Username != "" || peer.OutboundAuth.Username != "" ||
			peer.OutboundAuth.Realm != "" || peer.AllowCallsFromPeer || peer.AllowSMSFromPeer || peer.AllowCallsToPeer || peer.AllowSMSToPeer ||
			len(peer.IncomingRoutes)+len(peer.OutgoingLines)+len(peer.SMSSendLines) != 0
		if peer.InboundAuth.Password != "" || peer.OutboundAuth.Password != "" ||
			configured && (current.PeerSecrets == nil || !validLocalCredentialEpoch(current.PeerSecrets.CredentialEpoch)) {
			return stored{}, errors.New("current durable SIP peer identity is invalid")
		}
		if configured && (peer.AllowCallsFromPeer || peer.AllowSMSFromPeer) && !validPeerSecret(current.PeerSecrets.Inbound) ||
			configured && (peer.AllowCallsFromPeer || peer.AllowCallsToPeer || peer.AllowSMSToPeer) && !validPeerSecret(current.PeerSecrets.Outbound) {
			return stored{}, errors.New("current durable SIP peer authentication is invalid")
		}
	}
	current.requireDurableAuthority = true
	return current, nil
}
