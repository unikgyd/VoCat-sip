package ims

import "vocat/internal/callrecord"

// callMu is held. The factory only enqueues creation; it must never perform
// filesystem work or call back into this IMS session.
func (session *Session) beginCallRecordingLocked(call *imsCall) {
	if call.recordingAttempted || call.terminated || call.public.State != "active" || call.answerSent && !call.answerAcked || call.media == nil || !call.media.ready() {
		return
	}
	call.recordingAttempted = true
	if session.provider == nil || session.provider.config.RecordCall == nil {
		return
	}
	access := "vowifi"
	if session.request.Cellular != nil {
		access = "host_ims"
	}
	sink := session.provider.config.RecordCall(callrecord.Call{DeviceID: session.request.DeviceID, ICCID: session.request.Identity.ICCID,
		CallID: call.public.ID, Number: call.public.Number, Direction: call.public.Direction, Access: access, CallStartedAt: call.public.StartedAt})
	call.media.attachRecording(sink)
}
