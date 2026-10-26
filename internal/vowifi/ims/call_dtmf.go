package ims

import "context"

func (session *Session) SendDTMF(ctx context.Context, id string, digit byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	session.callMu.Lock()
	call := session.calls[id]
	if call == nil {
		session.callMu.Unlock()
		return ErrCallNotFound
	}
	if call.terminated || call.public.EndedAt != nil || call.public.State != "active" || call.media == nil || !call.media.ready() {
		session.callMu.Unlock()
		return ErrCallState
	}
	media, lifetime := call.media, session.callMediaContextLocked(call)
	session.callMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, cancel)
	defer func() { stop(); cancel() }()
	return media.SendDTMF(ctx, digit)
}
