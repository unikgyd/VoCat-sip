package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"vocat/internal/developer"
	"vocat/internal/device"
	"vocat/internal/smsdecode"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type imsSMSController interface {
	SendSMS(context.Context, string, vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error)
}

func (s *Server) handleSMSSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"auto_clear_modem_storage": developer.AutoClearModemStorage(r.Context(), s.store),
		}})
	case http.MethodPut:
		var request struct {
			AutoClearModemStorage *bool `json:"auto_clear_modem_storage"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if request.AutoClearModemStorage == nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "auto_clear_modem_storage is required")
			return
		}
		if err := developer.SetAutoClearModemStorage(r.Context(), s.store, *request.AutoClearModemStorage); err != nil {
			s.writeStoreError(w, err)
			return
		}
		s.recordAudit(r.Context(), "admin", "settings.sms.auto_clear_modem_storage", "settings", "sms", "success", "modem SMS auto-clear updated")
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"auto_clear_modem_storage": developer.AutoClearModemStorage(r.Context(), s.store),
		}})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) routeSMSAPI(w http.ResponseWriter, r *http.Request, cleanPath string) bool {
	switch cleanPath {
	case "sms/diagnostics":
		s.handleSMSDiagnostics(w, r)
	case "sms/export":
		s.handleSMSExport(w, r)
	case "sms/contacts":
		s.handleSMSContacts(w, r)
	case "sms/thread":
		s.handleSMSThread(w, r)
	case "sms/send":
		s.handleSMSSend(w, r)
	default:
		segments := splitAPIPath(cleanPath)
		if len(segments) == 3 && segments[0] == "sms" && segments[1] == "messages" {
			s.handleSMSMessage(w, r, segments[2])
			return true
		}
		return false
	}
	return true
}

func (s *Server) handleSMSContacts(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	deviceID := normalizeSMSDeviceFilter(r.URL.Query().Get("device_id"))
	filter := s.smsStoreFilter(r.Context(), deviceID, "")
	filter.Limit = queryLimit(r, 100)
	contacts, err := s.store.ListSMSContacts(r.Context(), filter)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	result := make([]map[string]any, 0, len(contacts))
	for _, contact := range contacts {
		result = append(result, map[string]any{
			"device_id":      contact.DeviceID,
			"device_name":    contact.DeviceName,
			"modem_imei":     contact.ModemIMEI,
			"iccid":          contact.ICCID,
			"imsi":           contact.IMSI,
			"local_phone":    contact.LocalPhone,
			"peer":           contact.Peer,
			"display_name":   contact.DisplayName,
			"last_message":   smsdecode.Preview(contact.LastMessage),
			"last_content":   smsdecode.Preview(contact.LastMessage),
			"last_timestamp": contact.LastTimestamp,
			"direction":      contact.Direction,
			"last_type":      "sms",
			"last_sms_id":    contact.LastSMSID,
			"unread_count":   contact.UnreadCount,
			"message_count":  contact.MessageCount,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": result})
}

func (s *Server) handleSMSThread(w http.ResponseWriter, r *http.Request) {
	deviceID := normalizeSMSDeviceFilter(r.URL.Query().Get("device_id"))
	modemIMEI := strings.TrimSpace(r.URL.Query().Get("modem_imei"))
	iccid := strings.TrimSpace(r.URL.Query().Get("iccid"))
	imsi := strings.TrimSpace(r.URL.Query().Get("imsi"))
	peer := strings.TrimSpace(r.URL.Query().Get("peer"))
	if peer == "" {
		writeError(w, http.StatusBadRequest, "invalid_peer", "SMS peer is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		filter := s.smsStoreFilter(r.Context(), deviceID, modemIMEI)
		filter.ICCID = iccid
		filter.IMSI = imsi
		filter.Peer = peer
		filter.Limit = queryLimit(r, 100)
		if beforeID, parseErr := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("before_id")), 10, 64); parseErr == nil && beforeID > 0 {
			filter.BeforeID = beforeID
		}
		messages, err := s.store.ListSMSMessages(r.Context(), filter)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		unreadIDs := make([]int64, 0, len(messages))
		for i := range messages {
			if !messages[i].Read && (messages[i].Direction == "inbound" || messages[i].Direction == "received") {
				messages[i].Read = true
				unreadIDs = append(unreadIDs, messages[i].ID)
			}
		}
		if len(unreadIDs) > 0 {
			if markErr := s.store.MarkSMSMessagesRead(r.Context(), unreadIDs); markErr != nil {
				s.logger.Warn("mark SMS messages read failed", "error", markErr)
			}
		}
		reverseSMS(messages)
		result := make([]map[string]any, 0, len(messages))
		for _, message := range messages {
			result = append(result, storedSMSResponse(message))
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": result})
	case http.MethodDelete:
		filter := s.smsStoreFilter(r.Context(), deviceID, modemIMEI)
		filter.ICCID = iccid
		filter.IMSI = imsi
		filter.Peer = peer
		filter.Limit = 1000
		messages, err := s.store.ListSMSMessages(r.Context(), filter)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		if len(messages) == 0 {
			writeError(w, http.StatusNotFound, "not_found", "SMS thread was not found")
			return
		}
		if err := s.deleteSMSMessages(r.Context(), messages); err != nil {
			s.writeDeviceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{"deleted": len(messages)},
		})
	default:
		w.Header().Set("Allow", "GET, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func normalizeSMSDeviceFilter(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "all") {
		return ""
	}
	return value
}

// smsStoreFilter resolves a mutable configured device ID to the modem's stable
// IMEI. The ID is still used to address the live modem, but persisted history
// remains attached to the same hardware after the user renames that ID.
func (s *Server) smsStoreFilter(ctx context.Context, deviceID, requestedIMEI string) store.SMSFilter {
	filter := store.SMSFilter{ModemIMEI: strings.TrimSpace(requestedIMEI)}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return filter
	}
	filter.ModemIMEI = ""
	filter.DeviceID = deviceID
	config, err := s.store.Device(ctx, deviceID)
	if err != nil {
		return filter
	}
	imei := strings.TrimSpace(config.ModemIMEI)
	if entry, _, present := s.physicalForConfig(config); present {
		imei = firstNonEmpty(
			snapshotString(entry.Snapshot, func(snapshot *device.Snapshot) string { return snapshot.IMEI }),
			imei,
		)
	}
	if imei != "" {
		filter.DeviceID = ""
		filter.ModemIMEI = imei
	}
	return filter
}

// blockedSMSDestination reports whether the recipient is in a barred country.
// Normalization mirrors the PDU/IMS paths so the block cannot be sidestepped by
// dropping the leading "+" or using a 00 international prefix.
func blockedSMSDestination(phone string) (bool, string) {
	var digits strings.Builder
	for _, c := range strings.TrimSpace(phone) {
		if c >= '0' && c <= '9' {
			digits.WriteRune(c)
		}
	}
	d := digits.String()
	if strings.HasPrefix(d, "00") {
		d = d[2:]
	}
	if strings.HasPrefix(d, "86") {
		return true, "SMS to +86 (China) destinations is not allowed"
	}
	return false, ""
}

func (s *Server) handleSMSSend(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if s.devices == nil {
		writeError(w, http.StatusServiceUnavailable, "device_manager_unavailable", "device manager is unavailable")
		return
	}
	var request struct {
		Phone    string `json:"phone"`
		Message  string `json:"message"`
		DeviceID string `json:"device_id"`
	}
	if err := s.decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	request.DeviceID = strings.TrimSpace(request.DeviceID)
	if request.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "device_required", "a sending device is required")
		return
	}
	lock := s.imsModeLock(request.DeviceID)
	lock.RLock()
	defer lock.RUnlock()
	if blocked, reason := blockedSMSDestination(request.Phone); blocked {
		writeError(w, http.StatusBadRequest, "blocked_destination", reason)
		return
	}
	// Validate the logical message before consuming a global send slot. Both
	// cellular AT and VoWiFi IMS use this same encoder/validator.
	if _, err := device.PrepareSMSSubmitTPDUs(request.Phone, request.Message); err != nil {
		s.writeDeviceError(w, err)
		return
	}
	config, err := s.store.Device(r.Context(), request.DeviceID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	entry, physicalID, present := s.physicalForConfig(config)
	if !s.requirePhysicalDevice(w, present) {
		return
	}
	limit := developer.SMSHourlyLimit(r.Context(), s.store)
	reservation, err := s.store.ReserveSMSSend(r.Context(), request.DeviceID, limit, time.Now().UTC())
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !reservation.Allowed {
		retryAfter := time.Until(reservation.ResetAt)
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		w.Header().Set("Retry-After", strconv.FormatInt(int64((retryAfter+time.Second-1)/time.Second), 10))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": apiError{
				Code:    "sms_rate_limited",
				Message: fmt.Sprintf("Global SMS limit reached: at most %d messages may be submitted in a rolling one-hour window.", reservation.Limit),
			},
			"data": map[string]any{
				"limit":       reservation.Limit,
				"used":        reservation.Used,
				"remaining":   reservation.Remaining,
				"reset_at":    reservation.ResetAt,
				"retry_after": int64((retryAfter + time.Second - 1) / time.Second),
			},
		})
		return
	}
	hostIMS := store.EffectiveIMSMode(config) == store.IMSModeHost
	if config.Telephony != nil && !store.AccessEnabled(config, store.EffectiveIMSMode(config)) {
		writeError(w, http.StatusConflict, "access_disabled", "the default SMS access is disabled")
		return
	}
	explicitIMS := config.IMSMode != "" && store.EffectiveIMSMode(config) != store.IMSModeModule
	if (config.VoWiFiEnabled || hostIMS) && s.vowifi != nil {
		state, stateErr := s.vowifi.State(request.DeviceID)
		sender, canSendIMS := s.vowifi.(imsSMSController)
		if stateErr == nil && state.IMSReady && state.SMSReady && canSendIMS {
			result, sendErr := sender.SendSMS(r.Context(), request.DeviceID, vowifi.SMSSubmitRequest{
				Recipient: request.Phone,
				Text:      request.Message,
			})
			if sendErr == nil || result.PartsAttempted > 0 || !errors.Is(sendErr, vowifi.ErrSMSNotReady) {
				s.writeIMSSMSSendResult(w, r, request.DeviceID, request.Message, entry, result, sendErr)
				return
			}
		}
	}
	if explicitIMS {
		writeError(w, http.StatusServiceUnavailable, "ims_sms_not_ready", "the selected IMS line is not ready for SMS; no modem submission was attempted")
		return
	}
	// Prefer VoWiFi IMS when it is ready. If IMS is unavailable, including on a
	// native OpenStick 410 registered on a roaming cellular network, use the
	// modem's discovered auxiliary AT port and the existing AT+CMGS path.
	result, sendErr := s.devices.SendSMS(
		r.Context(),
		physicalID,
		request.Phone,
		request.Message,
	)
	if sendErr != nil && result.PartsAttempted == 0 {
		s.writeDeviceError(w, sendErr)
		return
	}
	identity := smsIdentityFromSnapshot(entry.Snapshot)
	modemIMEI := firstNonEmpty(
		snapshotString(entry.Snapshot, func(snapshot *device.Snapshot) string { return snapshot.IMEI }),
		config.ModemIMEI,
	)
	extra, _ := json.Marshal(map[string]any{
		"encoding":             result.Encoding,
		"message_reference":    result.MessageReference,
		"reference_known":      result.ReferenceKnown,
		"accepted_by_modem":    result.AcceptedByModem,
		"delivery_confirmed":   result.DeliveryConfirmed,
		"submission_status":    result.SubmissionStatus,
		"modem_final":          result.ModemFinal,
		"modem_evidence_count": len(result.ModemEvidence),
		"parts_total":          result.PartsTotal,
		"parts_attempted":      result.PartsAttempted,
		"parts_accepted":       result.PartsAccepted,
		"all_parts_accepted":   result.AllPartsAccepted,
		"concat_reference":     result.ConcatReference,
		"part_results":         result.PartResults,
	})
	messageID := fmt.Sprintf(
		"at-submit:%s:%d:%d",
		firstNonEmpty(modemIMEI, request.DeviceID),
		result.MessageReference,
		result.SubmittedAt.UnixNano(),
	)
	saved, err := s.store.SaveSMSMessage(r.Context(), store.SMSMessage{
		MessageID:     messageID,
		DeviceID:      request.DeviceID,
		ModemIMEI:     modemIMEI,
		ICCID:         identity.ICCID,
		IMSI:          identity.IMSI,
		LocalPhone:    s.smsLocalPhone(r.Context(), request.DeviceID, identity, entry.Snapshot),
		Peer:          result.To,
		Direction:     "outbound",
		Body:          request.Message,
		Timestamp:     result.SubmittedAt,
		Status:        result.SubmissionStatus,
		Source:        "cellular_at",
		PartsTotal:    result.PartsTotal,
		DeliveryState: result.DeliveryStatus,
		Read:          true,
		Extra:         extra,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	data := map[string]any{
		"message_id":          saved.MessageID,
		"id":                  saved.ID,
		"parts_total":         saved.PartsTotal,
		"parts_attempted":     result.PartsAttempted,
		"parts_accepted":      result.PartsAccepted,
		"all_parts_accepted":  result.AllPartsAccepted,
		"concat_reference":    result.ConcatReference,
		"part_results":        result.PartResults,
		"delivery_state":      saved.DeliveryState,
		"submission_state":    saved.Status,
		"message_reference":   result.MessageReference,
		"reference_known":     result.ReferenceKnown,
		"submission_accepted": result.AllPartsAccepted,
		"delivery_confirmed":  result.DeliveryConfirmed,
		"outcome":             smsSendOutcome(result.AllPartsAccepted, result.PartsAccepted, result.PartsTotal, result.DeliveryConfirmed),
		"transport":           "cellular_at",
	}
	if sendErr != nil {
		data["retry_safe"] = false
		if result.PartsAccepted > 0 {
			s.logger.Warn("multipart SMS was only partially accepted",
				"category", "sms", "event", "sms.submission",
				"device_id", request.DeviceID, "peer", request.Phone,
				"transport", "cellular_at", "parts_attempted", result.PartsAttempted,
				"parts_accepted", result.PartsAccepted, "raw_error", sendErr,
			)
			data["warning"] = "Only part of the multipart SMS was accepted by the modem. Do not retry the whole message."
			writeJSON(w, http.StatusAccepted, map[string]any{"data": data})
			return
		}
		s.logger.Warn(
			"SMS submission failed after modem interaction",
			"category", "sms",
			"event", "sms.submission",
			"device_id", request.DeviceID,
			"peer", request.Phone,
			"transport", "cellular_at",
			"parts_attempted", result.PartsAttempted,
			"parts_accepted", result.PartsAccepted,
			"raw_error", sendErr,
		)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": apiError{
				Code:    "sms_submission_failed",
				Message: "The modem did not provide complete proof that the SMS was accepted. Inspect part_results before retrying.",
			},
			"data": data,
		})
		return
	}
	if !result.AllPartsAccepted {
		s.logger.Warn("SMS submission was not confirmed",
			"category", "sms", "event", "sms.submission",
			"device_id", request.DeviceID, "peer", request.Phone,
			"transport", "cellular_at", "modem_final", result.ModemFinal,
			"parts_attempted", result.PartsAttempted, "parts_accepted", result.PartsAccepted,
		)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": apiError{
				Code:    "sms_submission_unconfirmed",
				Message: "The modem did not confirm acceptance of every SMS part.",
			},
			"data": data,
		})
		return
	}
	s.logger.Info("SMS submission accepted",
		"category", "sms", "event", "sms.submission",
		"device_id", request.DeviceID, "peer", request.Phone,
		"transport", "cellular_at", "parts", result.PartsAccepted,
	)
	writeJSON(w, http.StatusAccepted, map[string]any{"data": data})
}

func (s *Server) writeIMSSMSSendResult(
	w http.ResponseWriter,
	r *http.Request,
	deviceID string,
	body string,
	entry device.Device,
	result vowifi.SMSSubmitResult,
	sendErr error,
) {
	if sendErr != nil && result.PartsAttempted == 0 {
		if errors.Is(sendErr, device.ErrSMSInvalidRecipient) ||
			errors.Is(sendErr, device.ErrSMSEmpty) ||
			errors.Is(sendErr, device.ErrSMSTooLong) {
			s.writeDeviceError(w, sendErr)
			return
		}
		writeError(w, http.StatusBadGateway, "ims_sms_submission_failed", sendErr.Error())
		return
	}
	extra, _ := json.Marshal(map[string]any{
		"transport":          "ims",
		"encoding":           result.Encoding,
		"parts_total":        result.PartsTotal,
		"parts_attempted":    result.PartsAttempted,
		"parts_accepted":     result.PartsAccepted,
		"all_parts_accepted": result.AllPartsAccepted,
		"concat_reference":   result.ConcatReference,
		"part_results":       result.PartResults,
		"delivery_confirmed": result.DeliveryConfirmed,
		"submission_status":  result.SubmissionStatus,
	})
	identity := smsIdentityFromSnapshot(entry.Snapshot)
	if s.vowifi != nil {
		if state, stateErr := s.vowifi.State(deviceID); stateErr == nil {
			identity.ICCID = strings.TrimSpace(state.ICCID)
			identity.IMSI = strings.TrimSpace(state.IMSI)
		}
	}
	modemIMEI := snapshotString(entry.Snapshot, func(snapshot *device.Snapshot) string { return snapshot.IMEI })
	if config, configErr := s.store.Device(r.Context(), deviceID); configErr == nil {
		modemIMEI = firstNonEmpty(modemIMEI, config.ModemIMEI)
	}
	saved, err := s.store.SaveSMSMessage(r.Context(), store.SMSMessage{
		MessageID:     fmt.Sprintf("ims-submit:%s:%d", firstNonEmpty(modemIMEI, deviceID), result.SubmittedAt.UnixNano()),
		DeviceID:      deviceID,
		ModemIMEI:     modemIMEI,
		ICCID:         identity.ICCID,
		IMSI:          identity.IMSI,
		LocalPhone:    s.smsLocalPhone(r.Context(), deviceID, identity, entry.Snapshot),
		Peer:          result.To,
		Direction:     "outbound",
		Body:          body,
		Timestamp:     result.SubmittedAt,
		Status:        result.SubmissionStatus,
		Source:        "ims",
		PartsTotal:    result.PartsTotal,
		DeliveryState: imsSMSDeliveryState(result),
		Read:          true,
		Extra:         extra,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	data := map[string]any{
		"message_id":          saved.MessageID,
		"id":                  saved.ID,
		"parts_total":         result.PartsTotal,
		"parts_attempted":     result.PartsAttempted,
		"parts_accepted":      result.PartsAccepted,
		"all_parts_accepted":  result.AllPartsAccepted,
		"concat_reference":    result.ConcatReference,
		"part_results":        result.PartResults,
		"delivery_state":      saved.DeliveryState,
		"submission_state":    saved.Status,
		"transport":           "ims",
		"submission_accepted": result.AllPartsAccepted,
		"delivery_confirmed":  result.DeliveryConfirmed,
		"outcome":             smsSendOutcome(result.AllPartsAccepted, result.PartsAccepted, result.PartsTotal, result.DeliveryConfirmed),
	}
	if sendErr != nil {
		s.logger.Warn("IMS SMS submission failed",
			"category", "sms", "event", "sms.submission",
			"device_id", deviceID, "peer", result.To, "transport", "ims",
			"parts_attempted", result.PartsAttempted, "parts_accepted", result.PartsAccepted,
			"raw_error", sendErr,
		)
		data["retry_safe"] = false
		data["warning"] = sendErr.Error()
		if result.PartsAccepted == 0 {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": apiError{
					Code:    "ims_sms_submission_failed",
					Message: "IMS did not accept the SMS submission.",
				},
				"data": data,
			})
			return
		}
	}
	if !result.AllPartsAccepted && result.PartsAccepted == 0 {
		s.logger.Warn("IMS SMS submission was not confirmed",
			"category", "sms", "event", "sms.submission",
			"device_id", deviceID, "peer", result.To, "transport", "ims",
			"parts_attempted", result.PartsAttempted, "parts_accepted", result.PartsAccepted,
		)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": apiError{
				Code:    "ims_sms_submission_unconfirmed",
				Message: "IMS did not confirm acceptance of every SMS part.",
			},
			"data": data,
		})
		return
	}
	if result.AllPartsAccepted {
		s.logger.Info("IMS SMS submission accepted",
			"category", "sms", "event", "sms.submission",
			"device_id", deviceID, "peer", result.To, "transport", "ims",
			"parts", result.PartsAccepted,
		)
	} else {
		s.logger.Warn("multipart IMS SMS was only partially accepted",
			"category", "sms", "event", "sms.submission",
			"device_id", deviceID, "peer", result.To, "transport", "ims",
			"parts_attempted", result.PartsAttempted, "parts_accepted", result.PartsAccepted,
		)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"data": data})
}

func smsSendOutcome(allAccepted bool, partsAccepted, partsTotal int, deliveryConfirmed bool) string {
	switch {
	case deliveryConfirmed:
		return "delivered"
	case allAccepted && partsTotal > 0 && partsAccepted == partsTotal:
		return "accepted_unconfirmed"
	case partsAccepted > 0:
		return "partial"
	default:
		return "failed"
	}
}

func imsSMSDeliveryState(result vowifi.SMSSubmitResult) string {
	switch smsSendOutcome(result.AllPartsAccepted, result.PartsAccepted, result.PartsTotal, result.DeliveryConfirmed) {
	case "delivered":
		return "delivered"
	case "accepted_unconfirmed":
		return "accepted_by_ims"
	case "partial":
		return "partial"
	default:
		return "failed"
	}
}

func (s *Server) handleSMSMessage(w http.ResponseWriter, r *http.Request, idText string) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_sms_id", "SMS message ID must be a positive integer")
		return
	}
	message, err := s.store.SMSMessage(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if err := s.deleteSMSMessages(r.Context(), []store.SMSMessage{message}); err != nil {
		s.writeDeviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"deleted": true}})
}

func (s *Server) syncModemSMS(ctx context.Context, onlyDevice string) {
	s.syncModemSMSWithObservation(ctx, onlyDevice, false)
}

// An observed scan is not proof of a network arrival. It only permits new
// physical rows after a full successful archive baseline in the same scope.
func (s *Server) syncObservedModemSMS(ctx context.Context, onlyDevice string) {
	s.syncModemSMSWithObservation(ctx, onlyDevice, true)
}

func (s *Server) syncModemSMSWithObservation(ctx context.Context, onlyDevice string, allowLive bool) {
	s.smsSyncMu.Lock()
	defer s.smsSyncMu.Unlock()
	if s.devices == nil {
		s.invalidateSMSForwardScans()
		return
	}
	configs, err := s.store.ListDevices(ctx)
	if err != nil {
		s.invalidateSMSForwardScans()
		s.logger.Warn("list devices for SMS synchronization failed", "error", err)
		return
	}
	configured := map[string]bool{}
	for _, config := range configs {
		configured[config.ID] = true
		if onlyDevice != "" && config.ID != onlyDevice {
			continue
		}
		s.syncOneModemSMSObservedLocked(ctx, config.ID, allowLive)
	}
	if onlyDevice != "" {
		if !configured[onlyDevice] {
			s.forgetSMSForwardScan(onlyDevice)
		}
	} else {
		for id := range s.smsForwardScans {
			if !configured[id] {
				s.forgetSMSForwardScan(id)
			}
		}
	}
}

// The caller owns smsSyncMu, but this line's mode gate is released before the
// next module is scanned, even when that next CMGL operation is slow.
func (s *Server) syncOneModemSMSLocked(ctx context.Context, id string) {
	s.syncOneModemSMSObservedLocked(ctx, id, false)
}

func (s *Server) syncOneModemSMSObservedLocked(ctx context.Context, id string, allowLive bool) {
	completed := false
	defer func() {
		if !completed {
			s.invalidateSMSForwardScan(id)
		}
	}()
	lock := s.imsModeLock(id)
	lock.RLock()
	defer lock.RUnlock()
	config, err := s.store.Device(ctx, id)
	if err != nil {
		return
	}
	if store.HostIMSConfigured(config) {
		// Host owns the modem's UIM/IMS lane even while registering/failed.
		// Do not run CNMI/CMGL or reset recovery underneath that owner.
		return
	}
	// A PC/SC USB reader has no modem storage or AT command channel. Its
	// messages are delivered by the active VoWiFi IMS session, so attempting
	// an AT+CMGL catch-up scan would only poison the reader's health state
	// with ErrNoATPort.
	if !supportsModemSMSStorage(config) {
		return
	}
	// Do not queue CMGL traffic on the same serial actor while VoWiFi is
	// reading the SIM or running AKA. Once the session is stable, resume the
	// SM/ME scan as a catch-up path: an SMS submitted while the card was
	// offline may be delivered to modem storage when it comes back, even
	// though subsequent live SMS is delivered by SIP MESSAGE.
	if s.vowifi != nil {
		state, stateErr := s.vowifi.State(config.ID)
		deferSync := shouldDeferModemSMSSync(state, stateErr)
		if !deferSync && stateErr == nil && state.Enabled && state.Phase == vowifi.PhaseFailed {
			if controller, ok := s.vowifi.(VoWiFiSMSSyncController); ok {
				deferSync = controller.ModemSMSSyncBlocked(config.ID)
			}
		}
		if deferSync {
			s.logger.Debug("modem SMS synchronization deferred during VoWiFi setup",
				"category", "sms", "device_id", config.ID, "phase", state.Phase)
			return
		}
	}
	entry, physicalID, present := s.physicalForConfig(config)
	if !present {
		return
	}
	identityBefore := smsIdentityFromSnapshot(entry.Snapshot)
	imeiBefore := strings.TrimSpace(snapshotString(entry.Snapshot, func(snapshot *device.Snapshot) string { return snapshot.IMEI }))
	// Store new SMS and announce them with +CMTI. Re-applied every scan:
	// firmware (e.g. OpenStick 410) can reset CNMI after a profile switch.
	setupContext, cancelSetup := context.WithTimeout(ctx, 5*time.Second)
	_, setupErr := s.devices.ExecuteAT(setupContext, physicalID, `AT+CNMI=2,1,0,0,0`)
	cancelSetup()
	if setupErr != nil {
		s.logger.Debug("cellular SMS notification setup skipped", "device_id", config.ID, "error", setupErr)
	}
	listContext, cancelList := context.WithTimeout(ctx, 30*time.Second)
	listing, err := s.devices.ListSMS(listContext, physicalID)
	cancelList()
	if err != nil {
		s.logger.Debug("modem SMS synchronization skipped", "device_id", config.ID, "error", err)
		return
	}
	s.rememberSMSStorage(config.ID, listing.Storage)
	messages := listing.Messages
	currentEntry, currentErr := s.devices.Get(physicalID)
	if currentErr != nil || !currentEntry.Discovered || currentEntry.ID != physicalID {
		s.logger.Debug("modem SMS synchronization lost device identity", "device_id", config.ID, "error", currentErr)
		return
	}
	identityAfter := smsIdentityFromSnapshot(currentEntry.Snapshot)
	imeiAfter := strings.TrimSpace(snapshotString(currentEntry.Snapshot, func(snapshot *device.Snapshot) string { return snapshot.IMEI }))
	if identityBefore != identityAfter || imeiBefore != imeiAfter {
		s.logger.Info(
			"modem SMS synchronization deferred after subscription identity changed",
			"category", "sms", "device_id", config.ID,
			"previous_iccid", identityBefore.ICCID, "current_iccid", identityAfter.ICCID,
		)
		return
	}
	localPhone := s.smsLocalPhone(ctx, config.ID, identityAfter, currentEntry.Snapshot)
	modemIMEI := firstNonEmpty(
		snapshotString(currentEntry.Snapshot, func(snapshot *device.Snapshot) string { return snapshot.IMEI }),
		config.ModemIMEI,
	)
	concatSources := modemSMSConcatSources(messages)
	scope := smsForwardScanScope{physicalID: physicalID, imei: imeiAfter, iccid: identityAfter.ICCID, imsi: identityAfter.IMSI, mode: store.EffectiveIMSMode(config)}
	decision, nextScan, trackable := smsForwardScanDecisions(s.smsForwardScans[id], scope, messages, concatSources, allowLive && !s.smsForwardScanBlocked)
	allSaved := true
	autoClear := developer.AutoClearModemStorage(ctx, s.store)
	var clearSlots []modemSMSSlot
	// Multipart IDs derive from the first segment's slot in this scan, so
	// keep segments in storage until complete or modemSMSIncompleteHoldLimit.
	concatSlots := make(map[string][]modemSMSSlot)
	concatComplete := make(map[string]bool)
	concatFirstSeen := make(map[string]time.Time)
	concatSafeToClear := make(map[string]bool)
	for _, message := range messages {
		if message.Direction != device.SMSDirectionStatusReport && firstNonEmpty(message.From, message.To) == "" {
			message.DecodeError = firstNonEmpty(message.DecodeError, "SMS sender address is empty")
		}
		// Archive before sender parsing, delivery-report matching, or inbox
		// persistence. None of those operations may discard the only PDU.
		archiveErr := s.store.SaveSMSRawRecord(ctx, store.SMSRawRecord{
			DeviceID: config.ID, ModemIMEI: modemIMEI, ICCID: identityAfter.ICCID,
			IMSI: identityAfter.IMSI, Storage: message.Storage, Slot: message.Index,
			RawPDU: message.RawPDU, RawUserData: message.RawUserData,
			Encoding: string(message.Encoding), DecodeError: message.DecodeError,
		})
		if archiveErr != nil {
			allSaved = false
			s.logger.Warn("archive modem SMS failed; keeping modem copy", "category", "sms",
				"device_id", config.ID, "storage", message.Storage, "index", message.Index, "error", archiveErr)
		}
		if message.Direction == device.SMSDirectionStatusReport &&
			message.MessageReference != nil && message.StatusCode != nil {
			_, applyErr := s.store.ApplySMSDeliveryReport(ctx, store.SMSDeliveryReport{
				DeviceID:          config.ID,
				ModemIMEI:         modemIMEI,
				IMSI:              identityAfter.IMSI,
				Peer:              message.To,
				Source:            "cellular_at",
				MessageReference:  *message.MessageReference,
				StatusCode:        *message.StatusCode,
				DeliveryState:     message.DeliveryStatus,
				ServiceCenterTime: message.ServiceCenterTimestamp,
				DischargeTime:     message.DischargeTimestamp,
				ReceivedAt:        time.Now().UTC(),
			})
			if applyErr != nil {
				if !errors.Is(applyErr, store.ErrNotFound) {
					allSaved = false
					s.logger.Warn("apply modem SMS delivery report failed", "device_id", config.ID, "error", applyErr)
				}
				continue
			}
			if autoClear && archiveErr == nil && message.DecodeError == "" {
				clearSlots = appendModemSMSSlot(clearSlots, message)
			}
			continue
		}
		peer := firstNonEmpty(message.From, message.To, "未知发送方")
		originalPeer := message.From
		if originalPeer == "" {
			originalPeer = message.To
		}
		if strings.TrimSpace(originalPeer) == "" {
			originalPeer = peer
		}
		timestamp := time.Now().UTC()
		if message.ServiceCenterTimestamp != nil {
			timestamp = message.ServiceCenterTimestamp.UTC()
		} else if message.DischargeTimestamp != nil {
			timestamp = message.DischargeTimestamp.UTC()
		}
		direction := "inbound"
		if message.Direction == device.SMSDirectionSubmitted {
			direction = "outbound"
		}
		messageID := modemSMSMessageID(message, modemIMEI, config.ID, peer, concatSources[modemSMSStorageKey(message)])
		extra, _ := json.Marshal(map[string]any{
			"modem_index":        message.Index,
			"storage":            message.Storage,
			"storage_status":     message.StorageStatus,
			"encoding":           message.Encoding,
			"concat":             message.Concat,
			"decode_error":       message.DecodeError,
			"status_code":        message.StatusCode,
			"message_reference":  message.MessageReference,
			"delivery_status":    message.DeliveryStatus,
			"data_coding_scheme": message.DataCodingScheme,
			// Every AT modem periodically rescans persistent SMS storage. Keep a
			// completed multipart row's cursor stable when a decoder presents a
			// segment differently on a later pass, otherwise notification
			// providers can treat the old message as newly inserted.
			"keep_durable_id_on_rescan": true,
		})
		value := store.SMSMessage{
			MessageID: messageID,
			DeviceID:  config.ID,
			ModemIMEI: modemIMEI,
			// The immutable origin must retain the actual snapshot spelling;
			// trimming an untrusted ICCID must not create a reply-capable card.
			ICCID:         snapshotString(currentEntry.Snapshot, func(snapshot *device.Snapshot) string { return snapshot.ICCID }),
			IMSI:          identityAfter.IMSI,
			LocalPhone:    localPhone,
			Peer:          originalPeer,
			Direction:     direction,
			Body:          message.Text,
			Timestamp:     timestamp,
			Status:        string(message.StorageStatus),
			Source:        "cellular_at",
			PartsTotal:    concatTotal(message.Concat),
			DeliveryState: message.DeliveryStatus,
			Read:          message.StorageStatus == device.SMSStatusReceivedRead,
			Extra:         extra,
		}
		// Every new storage scope and every explicit/manual catch-up is an
		// archive. Old seen rows never gain new forwarding recipients, even if
		// their Inbox was deleted or the bounded source receipt has expired.
		var saveResult store.SMSMessageSaveResult
		var saveErr error
		if trackable && message.Direction == device.SMSDirectionReceived && decision[modemSMSScanRowKey(message)] && archiveErr == nil {
			saveResult, saveErr = s.store.SaveSMSMessageWithResult(ctx, value)
		} else {
			saveResult, saveErr = s.store.SaveSMSMessageArchiveWithResult(ctx, value)
		}
		if saveErr != nil {
			allSaved = false
			if message.Concat != nil && message.Concat.Total > 1 {
				concatSafeToClear[messageID] = false
			}
			s.logger.Warn("persist modem SMS failed", "category", "sms", "device_id", config.ID, "raw_error", saveErr)
			continue
		}
		if autoClear {
			safeToClear := archiveErr == nil && message.DecodeError == "" && strings.TrimSpace(message.Text) != ""
			if message.Concat != nil && message.Concat.Total > 1 {
				if _, seen := concatSafeToClear[messageID]; !seen {
					concatSafeToClear[messageID] = true
				}
				concatSafeToClear[messageID] = concatSafeToClear[messageID] && safeToClear && store.SMSDecodeError(saveResult.Message.Extra) == ""
				concatSlots[messageID] = appendModemSMSSlot(concatSlots[messageID], message)
				concatComplete[messageID] = store.ConcatSMSReadyToNotify(saveResult.Message.MessageID, saveResult.Message.Extra)
				// Kept across merges and rescans; unlike SCTS never missing or skewed.
				concatFirstSeen[messageID] = saveResult.Message.CreatedAt
			} else if safeToClear {
				clearSlots = appendModemSMSSlot(clearSlots, message)
			}
		}
		if saved := saveResult.Message; saveResult.Inserted && saved.Direction == "inbound" && smsMessageReadyToNotify(saved) {
			s.logger.Info("cellular SMS received",
				"category", "sms", "event", "sms.received", "sms_id", saved.ID,
				"device_id", config.ID, "peer", saved.Peer,
				"transport", "cellular_at", "encoding", message.Encoding,
				"parts", saved.PartsTotal,
			)
		}
	}
	// Even a failed persistence pass retains archive-family protection. It
	// never establishes a live baseline, but forgetting a seen old partial
	// would allow a later storage-generation change to give it new recipients.
	if !allSaved || ctx.Err() != nil || !trackable {
		nextScan.ready = false
		nextScan.rows = nil
	}
	s.rememberSMSForwardScan(id, nextScan)
	completed = true
	if autoClear {
		for messageID, slots := range concatSlots {
			if !concatSafeToClear[messageID] {
				continue
			}
			if concatComplete[messageID] {
				clearSlots = append(clearSlots, slots...)
			} else if time.Since(concatFirstSeen[messageID]) > modemSMSIncompleteHoldLimit {
				s.logger.Info("clearing incomplete multipart SMS from modem storage",
					"category", "sms", "device_id", config.ID, "segments", len(slots),
					"first_seen", concatFirstSeen[messageID])
				clearSlots = append(clearSlots, slots...)
			}
		}
		s.clearPersistedModemSMS(ctx, physicalID, config.ID, clearSlots)
	}
}

const (
	smsForwardScanScopeLimit = 512
	smsForwardScanRowLimit   = 4096
	smsForwardScanRowBudget  = 16384
	smsForwardScanFamilyHold = 24 * time.Hour
)

type smsForwardScanScope struct{ physicalID, imei, iccid, imsi, mode string }
type smsForwardScanRow struct{ archive bool }
type smsForwardScan struct {
	scope smsForwardScanScope
	rows  map[string]smsForwardScanRow
	ready bool
	// Family keys bind the scope and a hash of sender/ref/total, never a body
	// or PDU. A repeated old partial must not renew its first-seen deadline.
	families   map[string]time.Time
	guardUntil time.Time
	blocked    bool
}

// Only hashes of bounded physical row identities survive the scan. No body or
// raw PDU is retained in these qualifications or included in logs.
func modemSMSScanRowKey(message device.SMSMessage) string {
	storage := strings.ToUpper(strings.TrimSpace(message.Storage))
	if (storage != "SM" && storage != "ME") || message.Index < 0 {
		return ""
	}
	pdu := strings.TrimSpace(message.RawPDU)
	if len(pdu) == 0 || len(pdu) > 1024 {
		return ""
	}
	decoded, err := hex.DecodeString(pdu)
	if err != nil || len(decoded) == 0 {
		return ""
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(storage + ":" + strconv.Itoa(message.Index) + ":"))
	_, _ = digest.Write(decoded)
	return hex.EncodeToString(digest.Sum(nil))
}

func smsForwardScanDecisions(previous smsForwardScan, scope smsForwardScanScope, messages []device.SMSMessage, concatSources map[string]string, allowLive bool) (map[string]bool, smsForwardScan, bool) {
	return smsForwardScanDecisionsAt(previous, scope, messages, concatSources, allowLive, time.Now().UTC())
}

func smsForwardScanDecisionsAt(previous smsForwardScan, scope smsForwardScanScope, messages []device.SMSMessage, concatSources map[string]string, allowLive bool, now time.Time) (map[string]bool, smsForwardScan, bool) {
	now = now.UTC()
	next := smsForwardScan{scope: scope, rows: map[string]smsForwardScanRow{}, families: map[string]time.Time{}, ready: true, blocked: previous.blocked}
	for key, expiry := range previous.families {
		if expiry.After(now) {
			next.families[key] = expiry
		}
	}
	if previous.guardUntil.After(now) {
		next.guardUntil = previous.guardUntil
	}
	identityKnown := scope.physicalID != "" && scope.imei != "" && scope.iccid != "" && scope.imsi != ""
	trackable := identityKnown
	if len(messages) > smsForwardScanRowLimit {
		// Do not retain unbounded physical rows or forget an untracked partial
		// and later treat its new storage generation as live. Local Inbox saves
		// still run; only forwarding is conservatively disabled this process.
		next.blocked = true
		next.ready = false
		return nil, next, false
	}
	// A missing/undecodable PDU cannot establish a physical baseline. If a
	// later decoder resolves that same stored row, it must first be archived,
	// not be mistaken for a newly arrived SMS solely because its hash changed.
	for _, message := range messages {
		if modemSMSScanRowKey(message) == "" {
			trackable = false
			break
		}
	}
	qualified := trackable && previous.ready && previous.scope == scope && !next.blocked && !next.guardUntil.After(now)
	if !qualified {
		previous.rows = nil
	}
	groupArchive := map[string]bool{}
	groupNew := map[string]bool{}
	groups := make([]string, len(messages))
	groupFamilies := map[string]string{}
	groupTotals := map[string]int{}
	groupSequences := map[string]map[int]bool{}
	for index, message := range messages {
		rowKey := modemSMSScanRowKey(message)
		peer := firstNonEmpty(message.From, message.To, "未知发送方")
		group := modemSMSMessageID(message, scope.imei, "", peer, concatSources[modemSMSStorageKey(message)])
		groups[index] = group
		old, seen := previous.rows[rowKey]
		if !qualified || !allowLive || rowKey == "" || seen && old.archive {
			groupArchive[group] = true
		}
		if !seen && rowKey != "" {
			groupNew[group] = true
		}
		if message.Concat != nil && message.Concat.Total > 1 {
			family := smsForwardScanFamilyKey(scope, message, peer)
			groupFamilies[group] = family
			groupTotals[group] = message.Concat.Total
			if groupSequences[group] == nil {
				groupSequences[group] = map[int]bool{}
			}
			if message.Concat.Sequence > 0 && message.Concat.Sequence <= message.Concat.Total {
				groupSequences[group][message.Concat.Sequence] = true
			}
			if next.families[family].After(now) {
				groupArchive[group] = true
			}
		}
	}
	for group, family := range groupFamilies {
		if !groupArchive[group] || len(groupSequences[group]) == groupTotals[group] {
			continue
		}
		if _, seen := next.families[family]; !seen {
			// An unchanged, already archived physical partial does not restart
			// its expired family window on every periodic scan.
			if previous.ready && previous.scope == scope && !groupNew[group] {
				continue
			}
			if len(next.families) >= smsForwardScanRowLimit {
				next.blocked = true
				continue
			}
			next.families[family] = now.Add(smsForwardScanFamilyHold)
		}
		if !identityKnown && next.families[family].After(next.guardUntil) {
			next.guardUntil = next.families[family]
		}
	}
	for group, family := range groupFamilies {
		if next.families[family].After(now) {
			groupArchive[group] = true
		}
	}
	decision := map[string]bool{}
	for index, message := range messages {
		rowKey := modemSMSScanRowKey(message)
		group := groups[index]
		if rowKey == "" {
			continue
		}
		// All parts of one newly advancing live logical message may merge as
		// live; marking its previously seen partial as archive would itself
		// create an empty source receipt before the new last part arrived. The
		// same is true of repeated live partials while waiting for a last part.
		incomplete := groupTotals[group] > 1 && len(groupSequences[group]) != groupTotals[group]
		archive := groupArchive[group] || next.blocked || next.guardUntil.After(now)
		decision[rowKey] = qualified && allowLive && (groupNew[group] || incomplete) && !archive
		next.rows[rowKey] = smsForwardScanRow{archive: archive}
	}
	if len(next.rows)+len(next.families) > smsForwardScanRowLimit {
		next.blocked = true
	}
	if next.blocked {
		next.rows = nil
		next.ready = false
		return nil, next, false
	}
	if !trackable {
		next.rows = nil
		next.ready = false
	}
	return decision, next, trackable
}

func smsForwardScanFamilyKey(scope smsForwardScanScope, message device.SMSMessage, peer string) string {
	encoded, _ := json.Marshal([]string{scope.physicalID, scope.imei, scope.iccid, scope.imsi, scope.mode, peer, strconv.Itoa(message.Concat.Reference), strconv.Itoa(message.Concat.Total)})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// These helpers require smsSyncMu. Invalidating a baseline must not discard a
// protected partial, including across Host/defer/read-error passes or card swaps.
func (s *Server) invalidateSMSForwardScan(id string) {
	if previous, exists := s.smsForwardScans[id]; exists {
		previous.ready = false
		previous.rows = nil
		s.smsForwardScans[id] = previous
	}
}

func (s *Server) invalidateSMSForwardScans() {
	for id := range s.smsForwardScans {
		s.invalidateSMSForwardScan(id)
	}
}

func (s *Server) forgetSMSForwardScan(id string) {
	previous := s.smsForwardScans[id]
	if previous.blocked || previous.guardUntil.After(time.Now().UTC()) {
		s.blockSMSForwardScans()
	}
	for _, expiry := range previous.families {
		if expiry.After(time.Now().UTC()) {
			s.blockSMSForwardScans()
			break
		}
	}
	delete(s.smsForwardScans, id)
}

func (s *Server) blockSMSForwardScans() {
	if !s.smsForwardScanBlocked && s.logger != nil {
		s.logger.Warn("AT SMS forwarding is archive-only until restart after bounded source protection was discarded; local Inbox capture remains enabled", "category", "sms")
	}
	s.smsForwardScanBlocked = true
}

func (s *Server) rememberSMSForwardScan(id string, next smsForwardScan) {
	if s.smsForwardScans == nil {
		s.smsForwardScans = map[string]smsForwardScan{}
	}
	delete(s.smsForwardScans, id)
	if next.blocked {
		s.blockSMSForwardScans()
	}
	total := len(next.rows) + len(next.families)
	for _, scan := range s.smsForwardScans {
		total += len(scan.rows) + len(scan.families)
	}
	for len(s.smsForwardScans) >= smsForwardScanScopeLimit || total > smsForwardScanRowBudget {
		for previous, scan := range s.smsForwardScans {
			s.forgetSMSForwardScan(previous)
			total -= len(scan.rows) + len(scan.families)
			break
		}
	}
	if s.smsForwardScanBlocked {
		// A hard budget fallback affects SIP plans only, never local capture.
		// It cannot silently recover after forgetting source protection.
		next.ready = false
		next.rows = nil
		next.families = nil
	}
	s.smsForwardScans[id] = next
}

type modemSMSSlot struct {
	storage string
	index   int
	rawPDU  string
}

func appendModemSMSSlot(slots []modemSMSSlot, message device.SMSMessage) []modemSMSSlot {
	storage := strings.ToUpper(strings.TrimSpace(message.Storage))
	if (storage != "SM" && storage != "ME") || message.Index < 0 {
		return slots
	}
	for _, existing := range slots {
		if existing.storage == storage && existing.index == message.Index {
			return slots
		}
	}
	return append(slots, modemSMSSlot{storage: storage, index: message.Index, rawPDU: message.RawPDU})
}

func sortModemSMSSlotsDescending(slots []modemSMSSlot) {
	sort.SliceStable(slots, func(i, j int) bool {
		if slots[i].storage != slots[j].storage {
			return slots[i].storage > slots[j].storage
		}
		return slots[i].index > slots[j].index
	})
}

func (s *Server) clearPersistedModemSMS(ctx context.Context, physicalID, configID string, slots []modemSMSSlot) {
	if len(slots) == 0 || s.devices == nil {
		return
	}
	sortModemSMSSlotsDescending(slots)
	for _, slot := range slots {
		deleteContext, cancelDelete := context.WithTimeout(ctx, 10*time.Second)
		err := s.devices.DeleteSMSFromStorageIfMatches(deleteContext, physicalID, slot.storage, slot.index, slot.rawPDU)
		cancelDelete()
		if err != nil {
			if s.logger != nil {
				s.logger.Warn(
					"clear persisted modem SMS failed",
					"category", "sms", "device_id", configID,
					"storage", slot.storage, "index", slot.index, "error", err,
				)
			}
			continue
		}
		s.noteSMSStorageFreed(configID, slot.storage)
	}
}

func modemSMSMessageID(message device.SMSMessage, modemIMEI, deviceID, peer, concatSource string) string {
	digest := sha256.Sum256([]byte(message.RawPDU))
	messageID := fmt.Sprintf(
		"modem:%s:%d:%s",
		message.Storage,
		message.Index,
		hex.EncodeToString(digest[:8]),
	)
	if message.Concat != nil && message.Concat.Total > 1 {
		// A segment of a carrier-split long SMS. Address the whole message
		// with a storage-generation id so SaveSMSMessage folds every segment
		// into one row without colliding with an older message that reused the
		// same UDH reference. SM and ME can expose duplicate copies of the same
		// slots, so the generation uses the first segment index, not storage.
		source := firstNonEmpty(concatSource, "cellular_at")
		messageID = store.StableConcatMessageID(
			source, modemIMEI, deviceID, peer,
			message.Concat.Reference, message.Concat.Total,
		)
	}
	return messageID
}

type smsSubscriptionIdentity struct {
	ICCID string
	IMSI  string
}

func smsIdentityFromSnapshot(snapshot *device.Snapshot) smsSubscriptionIdentity {
	if snapshot == nil {
		return smsSubscriptionIdentity{}
	}
	return smsSubscriptionIdentity{
		ICCID: strings.TrimSpace(snapshot.ICCID),
		IMSI:  strings.TrimSpace(snapshot.IMSI),
	}
}

func (s *Server) smsLocalPhone(
	ctx context.Context,
	deviceID string,
	identity smsSubscriptionIdentity,
	snapshot *device.Snapshot,
) string {
	if identity.ICCID != "" {
		if number, err := s.store.PhoneNumberForICCID(ctx, identity.ICCID); err == nil {
			return number
		} else if !errors.Is(err, store.ErrNotFound) {
			s.logger.Debug("read SMS phone association failed", "iccid", identity.ICCID, "error", err)
		}
	}
	if s.vowifi != nil {
		if state, err := s.vowifi.State(strings.TrimSpace(deviceID)); err == nil &&
			strings.TrimSpace(state.ICCID) == identity.ICCID {
			if number := strings.TrimSpace(state.PhoneNumber); number != "" {
				return number
			}
		}
	}
	if snapshot != nil && strings.TrimSpace(snapshot.ICCID) == identity.ICCID {
		return strings.TrimSpace(snapshot.Phone.Number)
	}
	return ""
}

// modemSMSConcatSources identifies every multipart group by the storage slot of
// its first segment. Carrier and modem storage can interleave unrelated SMS
// between segments, so deriving that slot through index arithmetic separates a
// single long SMS into several rows. The real first-segment slot also keeps
// messages that reuse the same UDH reference in distinct groups.
func modemSMSConcatSources(messages []device.SMSMessage) map[string]string {
	firstSlots := make(map[string][]int)
	for _, message := range messages {
		if message.Concat == nil || message.Concat.Total <= 1 || message.Concat.Sequence != 1 {
			continue
		}
		peer := firstNonEmpty(message.From, message.To)
		if peer == "" {
			continue
		}
		key := modemSMSConcatKey(message, peer)
		firstSlots[key] = append(firstSlots[key], message.Index)
	}
	for key := range firstSlots {
		sort.Ints(firstSlots[key])
	}

	result := make(map[string]string)
	for _, message := range messages {
		if message.Concat == nil || message.Concat.Total <= 1 {
			continue
		}
		peer := firstNonEmpty(message.From, message.To)
		if peer == "" {
			continue
		}
		source := ""
		for _, firstSlot := range firstSlots[modemSMSConcatKey(message, peer)] {
			if firstSlot > message.Index {
				break
			}
			source = fmt.Sprintf("cellular_at:%d", firstSlot)
		}
		if source == "" {
			source = fmt.Sprintf("cellular_at:pending:%d", message.Index)
		}
		result[modemSMSStorageKey(message)] = source
	}
	return result
}

func modemSMSConcatKey(message device.SMSMessage, peer string) string {
	return peer + ":" + strconv.Itoa(message.Concat.Reference) + ":" + strconv.Itoa(message.Concat.Total)
}

func modemSMSStorageKey(message device.SMSMessage) string {
	return message.Storage + ":" + strconv.Itoa(message.Index)
}

func (s *Server) deleteSMSMessages(ctx context.Context, messages []store.SMSMessage) error {
	s.smsSyncMu.Lock()
	defer s.smsSyncMu.Unlock()
	for _, message := range messages {
		if err := s.deleteModemSMS(ctx, message); err != nil {
			return err
		}
	}
	for _, message := range messages {
		if err := s.store.DeleteSMSMessage(ctx, message.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) deleteModemSMS(ctx context.Context, stored store.SMSMessage) error {
	if stored.Source != "cellular_at" ||
		(!strings.HasPrefix(stored.MessageID, "modem:") && !strings.HasPrefix(stored.MessageID, store.ConcatMessageIDPrefix)) {
		return nil
	}
	if s.devices == nil {
		return errors.New("device manager is unavailable for modem SMS deletion")
	}
	configs, err := s.store.ListDevices(ctx)
	if err != nil {
		return fmt.Errorf("list devices for modem SMS deletion: %w", err)
	}
	var config store.Device
	found := false
	for _, candidate := range configs {
		if stored.ModemIMEI != "" && candidate.ModemIMEI == stored.ModemIMEI {
			config, found = candidate, true
			break
		}
	}
	if !found {
		for _, candidate := range configs {
			if candidate.ID == stored.DeviceID {
				config, found = candidate, true
				break
			}
		}
	}
	if !found {
		// Removed devices leave SMS history that can be deleted locally.
		return nil
	}
	lock := s.imsModeLock(config.ID)
	lock.RLock()
	defer lock.RUnlock()
	config, err = s.store.Device(ctx, config.ID)
	if err != nil {
		return err
	}
	if store.HostIMSConfigured(config) {
		// History remains removable, but legacy modem storage reads/deletes may
		// not borrow the SIM/control channel from a Host owner.
		return nil
	}
	_, physicalID, present := s.physicalForConfig(config)
	if !present {
		return device.ErrNotFound
	}
	listContext, cancelList := context.WithTimeout(ctx, 30*time.Second)
	listing, err := s.devices.ListSMS(listContext, physicalID)
	cancelList()
	if err != nil {
		return fmt.Errorf("list modem SMS before deletion: %w", err)
	}
	s.rememberSMSStorage(config.ID, listing.Storage)
	modemMessages := listing.Messages
	concatSources := modemSMSConcatSources(modemMessages)
	locations := make(map[string]device.SMSMessage)
	for _, message := range modemMessages {
		peer := firstNonEmpty(message.From, message.To)
		if peer == "" || modemSMSMessageID(message, stored.ModemIMEI, config.ID, peer, concatSources[modemSMSStorageKey(message)]) != stored.MessageID {
			continue
		}
		locations[modemSMSStorageKey(message)] = message
	}
	for _, message := range locations {
		deleteContext, cancelDelete := context.WithTimeout(ctx, 10*time.Second)
		err := s.devices.DeleteSMSFromStorage(deleteContext, physicalID, message.Storage, message.Index)
		cancelDelete()
		if err != nil {
			return fmt.Errorf("delete modem SMS from %s index %d: %w", message.Storage, message.Index, err)
		}
	}
	return nil
}

func supportsModemSMSStorage(config store.Device) bool {
	deviceType := store.NormalizeDeviceType(config.DeviceType)
	if deviceType == store.DeviceTypeUSBSIMReader {
		return false
	}
	if deviceType == store.DeviceTypeWiFi410 {
		return strings.TrimSpace(config.ATPort) != ""
	}
	return true
}

func shouldDeferModemSMSSync(state vowifi.State, stateErr error) bool {
	if stateErr != nil || !state.Enabled {
		return false
	}
	// Setup phases own the UICC/AT path. Once IMS SMS is stable, allow the modem
	// storage catch-up scan. A failed phase is also eligible unless the runtime's
	// separate busy/retry signal says an automatic recovery currently owns AT.
	switch state.Phase {
	case vowifi.PhaseSMSReady, vowifi.PhaseFailed:
		return false
	default:
		return true
	}
}

// modemSMSIncompleteHoldLimit bounds how long segments of an incomplete
// multipart SMS occupy modem storage after VoCat first stored them, so a
// segment the carrier lost cannot fill the limited slots forever.
const modemSMSIncompleteHoldLimit = 24 * time.Hour

type cellularSMSArrivalSource interface {
	SMSArrivals() <-chan struct{}
}

// StartSMSSyncLoop persists inbound cellular SMS even when no client has the
// SMS page open. A +CMTI from any modem triggers an immediate scan; the
// periodic tick is the safety net. The first tick is delayed so startup
// SIM/AKA work gets exclusive use of the modem. Stable VoWiFi sessions still
// scan SM and ME as a catch-up path for messages delivered while offline.
func (s *Server) StartSMSSyncLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	var arrivals <-chan struct{}
	if source, ok := s.devices.(cellularSMSArrivalSource); ok {
		arrivals = source.SMSArrivals()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncObservedModemSMS(ctx, "")
		case <-arrivals:
			// One arriving during the scan stays buffered for another pass.
			started := time.Now()
			s.syncObservedModemSMS(ctx, "")
			s.logger.Debug("modem SMS scan after +CMTI finished",
				"category", "sms", "duration", time.Since(started))
		}
	}
}

func storedSMSResponse(message store.SMSMessage) map[string]any {
	body := smsdecode.Preview(message.Body)
	decodeError := store.SMSDecodeError(message.Extra)
	decodeStatus := "decoded"
	if decodeError != "" {
		decodeStatus = "failed"
	} else if strings.TrimSpace(body) == "" {
		decodeStatus = "empty"
	}
	return map[string]any{
		"id":             message.ID,
		"message_id":     message.MessageID,
		"device_id":      message.DeviceID,
		"modem_imei":     message.ModemIMEI,
		"iccid":          message.ICCID,
		"imsi":           message.IMSI,
		"local_phone":    message.LocalPhone,
		"peer":           message.Peer,
		"direction":      message.Direction,
		"body":           body,
		"content":        body,
		"decode_status":  decodeStatus,
		"decode_error":   decodeError,
		"sender":         ternaryString(message.Direction == "outbound", "", message.Peer),
		"recipient":      ternaryString(message.Direction == "outbound", message.Peer, ""),
		"type":           "sms",
		"timestamp":      message.Timestamp,
		"status":         message.Status,
		"source":         message.Source,
		"parts_total":    message.PartsTotal,
		"delivery_state": message.DeliveryState,
	}
}

func reverseSMS(messages []store.SMSMessage) {
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
}

func concatTotal(value *device.SMSConcatInfo) int {
	if value == nil || value.Total < 1 {
		return 1
	}
	return value.Total
}

func ternaryString(condition bool, yes string, no string) string {
	if condition {
		return yes
	}
	return no
}

func queryLimit(r *http.Request, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || value < 1 {
		return fallback
	}
	if value > 1000 {
		return 1000
	}
	return value
}

func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "the requested record was not found")
		return
	}
	s.logger.Error("database operation failed", "category", "system", "event", "store.operation_failed", "raw_error", err)
	writeError(w, http.StatusInternalServerError, "database_error", "the database operation failed")
}

func (s *Server) rememberSMSStorage(configID string, usage device.SMSStorageUsage) {
	if s == nil || !usage.Known() {
		return
	}
	s.smsStorageMu.Lock()
	defer s.smsStorageMu.Unlock()
	if s.smsStorage == nil {
		s.smsStorage = make(map[string]device.SMSStorageUsage)
	}
	s.smsStorage[configID] = usage
}

func (s *Server) smsStorageUsage(configID string) (device.SMSStorageUsage, bool) {
	if s == nil {
		return device.SMSStorageUsage{}, false
	}
	s.smsStorageMu.Lock()
	defer s.smsStorageMu.Unlock()
	usage, ok := s.smsStorage[configID]
	return usage, ok && usage.Known()
}

func (s *Server) noteSMSStorageFreed(configID, storage string) {
	if s == nil {
		return
	}
	s.smsStorageMu.Lock()
	defer s.smsStorageMu.Unlock()
	usage, ok := s.smsStorage[configID]
	if !ok {
		return
	}
	switch strings.ToUpper(strings.TrimSpace(storage)) {
	case "SM":
		if usage.SM.Used > 0 {
			usage.SM.Used--
		}
	case "ME":
		if usage.ME.Used > 0 {
			usage.ME.Used--
		}
	default:
		return
	}
	s.smsStorage[configID] = usage
}
