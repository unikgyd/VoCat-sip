package server

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"vocat/internal/callprompt"
	"vocat/internal/device"
	"vocat/internal/store"
)

func (s *Server) handleAutoAnswer(w http.ResponseWriter, r *http.Request, config store.Device, prompt bool) bool {
	w.Header().Set("Cache-Control", "no-store")
	identity, err := s.observedTelephonySubscription(r.Context(), config)
	entry, _, present := s.physicalForConfig(config)
	generation := entry.Candidate.USBGeneration
	if err != nil || !present || identity.ICCID == "" || config.Telephony != nil && config.Telephony.ICCID != identity.ICCID {
		if r.Method == http.MethodGet && !prompt {
			writeJSON(w, 200, map[string]any{"data": map[string]any{"settings": nil, "compressed_available": callprompt.CompressedAvailable()}})
		} else {
			writeError(w, 409, "sim_identity_unavailable", "the live SIM must be verified before accessing answering settings")
		}
		return true
	}
	current, err := s.store.CallAnswering(r.Context(), identity.ICCID)
	if err != nil {
		s.writeStoreError(w, err)
		return true
	}
	respond := func(a store.CallAnswering) {
		writeJSON(w, 200, map[string]any{"data": map[string]any{"settings": a, "compressed_available": callprompt.CompressedAvailable()}})
	}
	if r.Method == http.MethodGet {
		if !prompt {
			respond(current)
			return true
		}
		if current.Prompt == nil || r.URL.Query().Get("revision") != current.Revision || r.URL.Query().Get("iccid") != current.ICCID {
			writeError(w, 409, "message_changed", "reload the current SIM message")
			return true
		}
		_, pcm, err := s.store.CallAnsweringAudio(r.Context(), current.ICCID, current.Revision)
		if err != nil {
			s.writeStoreError(w, err)
			return true
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Disposition", `inline; filename="message.wav"`)
		_, _ = w.Write(callprompt.WAV(pcm))
		return true
	}
	requested := current
	var replacement *callprompt.Recording
	if prompt && r.Method == http.MethodPost {
		if !s.callPromptImport.TryLock() {
			writeError(w, 409, "message_import_busy", "another message is being imported")
			return true
		}
		defer s.callPromptImport.Unlock()
		r.Body = http.MaxBytesReader(w, r.Body, callprompt.MaxUploadBytes+(1<<20))
		reader, err := r.MultipartReader()
		if err != nil {
			writeError(w, 400, "invalid_upload", "use a multipart audio upload")
			return true
		}
		fields := make(map[string]string)
		var content []byte
		var filename string
		for count := 0; ; count++ {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil || count >= 3 {
				writeError(w, 400, "invalid_upload", "expected one audio file, ICCID and revision")
				return true
			}
			name := part.FormName()
			if name == "file" {
				if content != nil || part.FileName() == "" {
					writeError(w, 400, "invalid_upload", "only one audio file is allowed")
					return true
				}
				content, err = io.ReadAll(io.LimitReader(part, callprompt.MaxUploadBytes+1))
				filename = part.FileName()
			} else if name == "iccid" || name == "revision" {
				if _, exists := fields[name]; exists {
					writeError(w, 400, "invalid_upload", "duplicate upload field")
					return true
				}
				var value []byte
				value, err = io.ReadAll(io.LimitReader(part, 257))
				if len(value) > 256 {
					err = callprompt.ErrFormat
				}
				fields[name] = string(value)
			} else {
				err = callprompt.ErrFormat
			}
			part.Close()
			if err != nil || len(content) > callprompt.MaxUploadBytes {
				writeError(w, 400, "invalid_upload", "audio upload is invalid or exceeds 64 MiB")
				return true
			}
		}
		if fields["iccid"] != current.ICCID || fields["revision"] != current.Revision {
			writeError(w, 409, "settings_changed", store.ErrAnsweringConflict.Error())
			return true
		}
		recording, err := callprompt.Decode(r.Context(), content, filename)
		if err != nil {
			writeError(w, 400, "invalid_message", err.Error())
			return true
		}
		replacement = &recording
		// Replacing a message requires the operator to enable it explicitly.
		requested.Enabled = false
	} else if (!prompt && r.Method == http.MethodPut) || (prompt && r.Method == http.MethodDelete) {
		if err := s.decodeJSON(w, r, &requested); err != nil {
			writeError(w, 400, "invalid_settings", err.Error())
			return true
		}
		if requested.ICCID != current.ICCID || requested.Revision != current.Revision {
			writeError(w, 409, "settings_changed", store.ErrAnsweringConflict.Error())
			return true
		}
		if prompt {
			requested = current
			replacement = &callprompt.Recording{}
		}
	} else {
		w.Header().Set("Allow", map[bool]string{true: "GET, POST, DELETE", false: "GET, PUT"}[prompt])
		writeError(w, 405, "method_not_allowed", "method not allowed")
		return true
	}
	if requested.Enabled && (identity.IMSI == "" || device.IsPlaceholderIMSI(identity.IMSI) || device.RegionBlockReason(identity.IMSI) != "") {
		writeError(w, 403, "subscription_unavailable", "this subscription is not eligible for automatic answering")
		return true
	}
	// Decode can take time. Neither a new SIM nor a new USB generation inherits
	// the previous browser's message, even if the device ID stayed unchanged.
	check, checkErr := s.observedTelephonySubscription(r.Context(), config)
	latest, _, stillPresent := s.physicalForConfig(config)
	if checkErr != nil || !stillPresent || check.ICCID != identity.ICCID || check.IMSI != identity.IMSI || latest.Candidate.USBGeneration != generation {
		writeError(w, 409, "sim_identity_changed", "SIM or modem changed during the operation")
		return true
	}
	saved, err := s.store.SaveCallAnswering(r.Context(), requested, current.Revision, replacement)
	if err != nil {
		if errors.Is(err, store.ErrAnsweringConflict) {
			writeError(w, 409, "settings_changed", err.Error())
		} else {
			writeError(w, 400, "invalid_settings", err.Error())
		}
		return true
	}
	if s.autoAnswer != nil {
		s.autoAnswer.cancelSubscription(saved.ICCID, saved.Revision)
	}
	action := "call.answering.update"
	if prompt {
		action = "call.answering.message"
	}
	s.recordAudit(r.Context(), "admin", action, "device", config.ID, "success", strings.TrimSpace(saved.ICCID))
	respond(saved)
	return true
}
