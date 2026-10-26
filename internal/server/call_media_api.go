package server

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"vocat/internal/store"
)

const maxCallMediaMessage = 16 << 10

// handleCallMedia upgrades an authenticated same-origin request to a binary
// PCM bridge. Each WebSocket message contains little-endian signed 16-bit,
// 8 kHz, mono samples. RTP and codec details remain inside the IMS provider.
func (s *Server) handleCallMedia(w http.ResponseWriter, r *http.Request, config store.Device) bool {
	if !requireMethod(w, r, http.MethodGet) {
		return true
	}
	callID := strings.TrimSpace(r.URL.Query().Get("call_id"))
	if callID == "" || len(callID) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_call_id", "call_id is required")
		return true
	}
	if transport := s.callTransportForConfig(config); transport != "vowifi" && transport != "host_ims" && !s.moduleVoiceCall(config.ID, callID) && !(config.Telephony != nil && s.ownerIMSCall(config.ID, callID) != nil) {
		writeError(w, 501, "call_media_unavailable", "the exact call has no owned media path")
		return true
	}
	if s.sipGateway != nil && s.sipGateway.CarrierOwnsMedia(config.ID, callID) {
		writeError(w, http.StatusConflict, "call_media_owned_by_sip", "this call's audio is connected to a SIP extension")
		return true
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var token string
	pcmProtocol := false
	for _, protocol := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		protocol = strings.TrimSpace(protocol)
		if protocol == "vocat-pcm" {
			pcmProtocol = true
		}
		if strings.HasPrefix(protocol, "lease-") {
			token = strings.TrimPrefix(protocol, "lease-")
		}
	}
	if s.webPhone == nil || !pcmProtocol {
		writeError(w, 409, "audio_lease_required", "start or answer the call from the web phone first")
		return true
	}
	lease, claimed := s.webPhone.attach(config.ID, callID, token, cancel)
	if !claimed {
		writeError(w, 409, "audio_lease_required", "audio is already owned or the web phone lease is missing")
		return true
	}
	defer s.webPhone.detach(lease)
	media, err := (sipCarrierBackend{s}).Media(ctx, config.ID, callID)
	if err != nil {
		writeError(w, http.StatusConflict, "call_media_unavailable", err.Error())
		return true
	}
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
		Subprotocols:    []string{"vocat-pcm"},
	})
	if err != nil {
		return true
	}
	connection.SetReadLimit(maxCallMediaMessage)
	defer connection.CloseNow()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingCtx, stop := context.WithTimeout(ctx, 3*time.Second)
				err := connection.Ping(pingCtx)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	downlink := make(chan error, 1)
	go func() {
		defer cancel()
		for {
			samples, readErr := media.ReadPCM(ctx)
			if readErr != nil {
				downlink <- readErr
				return
			}
			payload := make([]byte, len(samples)*2)
			for index, sample := range samples {
				binary.LittleEndian.PutUint16(payload[index*2:], uint16(sample))
			}
			writeCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			writeErr := connection.Write(writeCtx, websocket.MessageBinary, payload)
			stop()
			if writeErr != nil {
				downlink <- writeErr
				return
			}
		}
	}()

	budget, last := 1600.0, time.Now()
	for {
		select {
		case err := <-downlink:
			if !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
				s.logger.Debug("call media downlink closed", "device_id", config.ID, "call_id", callID, "error", err)
			}
			return true
		default:
		}
		messageType, payload, readErr := connection.Read(ctx)
		if readErr != nil {
			return true
		}
		if messageType != websocket.MessageBinary || len(payload) == 0 || len(payload)%2 != 0 || len(payload) > 640 {
			return true
		}
		now := time.Now()
		budget = min(1600, budget+now.Sub(last).Seconds()*8000)
		last = now
		budget -= float64(len(payload) / 2)
		if budget < 0 {
			return true
		} // Bound queued uplink; never replay seconds of stale speech.
		samples := make([]int16, len(payload)/2)
		for index := range samples {
			samples[index] = int16(binary.LittleEndian.Uint16(payload[index*2:]))
		}
		if err := media.WritePCM(samples); err != nil {
			return true
		}
	}
}
