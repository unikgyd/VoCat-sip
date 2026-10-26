package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"vocat/internal/callprompt"
)

var ErrAnsweringConflict = errors.New("call answering settings changed; reload before saving")

// CallAnswering is a per-subscription preference. Audio is private database
// content, never a server-side path supplied by a browser or another SIM.
type CallAnswering struct {
	ICCID               string                `json:"iccid"`
	Revision            string                `json:"revision"`
	Enabled             bool                  `json:"enabled"`
	DelaySeconds        int                   `json:"delay_seconds"`
	HangupAfterPlayback bool                  `json:"hangup_after_playback"`
	MaxCallSeconds      int                   `json:"max_call_seconds"`
	Prompt              *callprompt.Recording `json:"prompt"`
}

func DefaultCallAnswering(iccid string) CallAnswering {
	return CallAnswering{ICCID: iccid, DelaySeconds: 30, HangupAfterPlayback: true, MaxCallSeconds: 600}
}
func (a CallAnswering) Validate() error {
	if len(a.ICCID) < 10 || len(a.ICCID) > 22 || strings.Trim(a.ICCID, "0123456789") != "" {
		return errors.New("a verified SIM ICCID is required")
	}
	if a.DelaySeconds < 5 || a.DelaySeconds > 120 {
		return errors.New("answer delay must be 5–120 seconds")
	}
	if a.MaxCallSeconds < 30 || a.MaxCallSeconds > 3600 {
		return errors.New("call duration limit must be 30–3600 seconds")
	}
	if a.Enabled && (a.Prompt == nil || a.Prompt.DurationMS <= 0) {
		return errors.New("upload a valid message before enabling automatic answering")
	}
	if a.Prompt != nil && (a.Prompt.DurationMS <= 0 || a.Prompt.DurationMS > callprompt.MaxSeconds*1000 || a.MaxCallSeconds*1000 < a.Prompt.DurationMS+5000) {
		return errors.New("call duration limit must exceed the message by at least five seconds")
	}
	return nil
}

func (s *Store) CallAnswering(ctx context.Context, iccid string) (CallAnswering, error) {
	a, _, err := s.loadCallAnswering(ctx, iccid, false)
	return a, err
}
func (s *Store) CallAnsweringAudio(ctx context.Context, iccid, revision string) (CallAnswering, []byte, error) {
	a, pcm, err := s.loadCallAnswering(ctx, iccid, true)
	if err == nil && a.Revision != revision {
		return a, nil, ErrAnsweringConflict
	}
	return a, pcm, err
}
func (s *Store) loadCallAnswering(ctx context.Context, iccid string, audio bool) (CallAnswering, []byte, error) {
	var raw, revision string
	var pcm []byte
	var err error
	if audio {
		err = s.db.QueryRowContext(ctx, `SELECT document_json,revision,pcm FROM call_answering WHERE iccid=?`, iccid).Scan(&raw, &revision, &pcm)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT document_json,revision FROM call_answering WHERE iccid=?`, iccid).Scan(&raw, &revision)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultCallAnswering(iccid), nil, nil
	}
	if err != nil {
		return CallAnswering{}, nil, err
	}
	var a CallAnswering
	if err = json.Unmarshal([]byte(raw), &a); err != nil {
		return a, nil, err
	}
	if a.ICCID != iccid || a.Revision != revision {
		return a, nil, errors.New("stored answering identity mismatch")
	}
	if err = a.Validate(); err != nil {
		return a, nil, err
	}
	if audio && a.Prompt != nil && (len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > callprompt.MaxPCMBytes || (len(pcm)/2*1000+7999)/8000 != a.Prompt.DurationMS) {
		return a, nil, errors.New("stored message is invalid")
	}
	return a, pcm, nil
}

// SaveCallAnswering atomically compares the revision. nil keeps the existing
// message; an empty recording clears it and disables answering.
func (s *Store) SaveCallAnswering(ctx context.Context, a CallAnswering, expected string, replacement *callprompt.Recording) (CallAnswering, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	var raw, oldRevision string
	var pcm []byte
	err = tx.QueryRowContext(ctx, `SELECT document_json,revision,pcm FROM call_answering WHERE iccid=?`, a.ICCID).Scan(&raw, &oldRevision, &pcm)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return a, err
	}
	if oldRevision != expected {
		return a, ErrAnsweringConflict
	}
	var old CallAnswering
	if raw != "" {
		if err = json.Unmarshal([]byte(raw), &old); err != nil {
			return a, err
		}
		if old.ICCID != a.ICCID {
			return a, errors.New("stored answering identity mismatch")
		}
	}
	a.Prompt = old.Prompt
	if replacement != nil {
		if len(replacement.PCM) == 0 {
			a.Enabled = false
			a.Prompt = nil
			pcm = []byte{}
		} else {
			if len(replacement.PCM)%2 != 0 || len(replacement.PCM) > callprompt.MaxPCMBytes {
				return a, callprompt.ErrFormat
			}
			metadata := *replacement
			metadata.PCM = nil
			metadata.DurationMS = (len(replacement.PCM)/2*1000 + 7999) / 8000
			a.Prompt = &metadata
			pcm = replacement.PCM
		}
	}
	if err = a.Validate(); err != nil {
		return a, err
	}
	if a.Enabled && len(pcm) == 0 {
		return a, errors.New("message audio is missing")
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return a, err
	}
	a.Revision = hex.EncodeToString(token[:])
	document, err := json.Marshal(a)
	if err != nil {
		return a, err
	}
	if pcm == nil {
		pcm = []byte{}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO call_answering(iccid,document_json,pcm,revision,updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(iccid) DO UPDATE SET document_json=excluded.document_json,pcm=excluded.pcm,revision=excluded.revision,updated_at=excluded.updated_at`, a.ICCID, string(document), pcm, a.Revision, time.Now().Unix())
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}
