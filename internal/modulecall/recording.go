package modulecall

import "vocat/internal/callrecord"

// The entry operation gate is held. Identity comes from the acquired lease,
// never from a later SIM observation or the current default outgoing route.
func (m *Manager) beginRecording(c *call) {
	if c.recordingAttempted {
		return
	}
	c.recordingAttempted = true
	if m.recordCall == nil {
		return
	}
	identity, ok := c.line.(interface{ RecordingICCID() string })
	if !ok {
		return
	}
	audio, ok := c.audio.(interface{ SetRecording(*callrecord.Sink) })
	if !ok {
		return
	}
	m.mu.Lock()
	view := c.public
	m.mu.Unlock()
	sink := m.recordCall(callrecord.Call{DeviceID: c.deviceID, ICCID: identity.RecordingICCID(), CallID: view.ID,
		Number: view.Number, Direction: view.Direction, Access: "module", CallStartedAt: view.StartedAt})
	audio.SetRecording(sink)
}
