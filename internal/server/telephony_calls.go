package server

import (
	"context"
	"vocat/internal/vowifi"
)

// The management surfaces share the same default-route admission and exact
// call ownership as the SIP/Web phone, including module calls on another access.
type telephonyCallController struct{ server *Server }

func (c telephonyCallController) Calls(id string) ([]vowifi.Call, error) {
	return (sipCarrierBackend{c.server}).Calls(context.Background(), id)
}
func (c telephonyCallController) DialCall(ctx context.Context, id, number string) (vowifi.Call, error) {
	return (sipCarrierBackend{c.server}).Dial(ctx, id, number)
}
func (c telephonyCallController) AnswerCall(ctx context.Context, id, call string) (vowifi.Call, error) {
	return (sipCarrierBackend{c.server}).Answer(ctx, id, call)
}
func (c telephonyCallController) HangupCall(ctx context.Context, id, call string) error {
	return (sipCarrierBackend{c.server}).Hangup(ctx, id, call)
}
