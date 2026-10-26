package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"vocat/internal/developer"
	"vocat/internal/device"
	"vocat/internal/sipgateway"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

type boundIMSSMSController interface {
	SendSMSForSubscription(context.Context, string, string, string, vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error)
}
type sipSMSBackend struct{ server *Server }

func smsSubscription(state vowifi.State) string {
	h := sha256.Sum256([]byte(state.ICCID))
	return hex.EncodeToString(h[:])
}
func (b sipSMSBackend) SMSReady(ctx context.Context, id string) (string, error) {
	if err := b.server.verifiedCarrierIMS(ctx, id); err != nil {
		return "", err
	}
	if _, ok := b.server.vowifi.(boundIMSSMSController); !ok {
		return "", sipgateway.ErrCarrierUnavailable
	}
	state, err := b.server.vowifi.State(id)
	if err != nil || !state.SMSReady {
		return "", sipgateway.ErrCarrierUnavailable
	}
	return smsSubscription(state), nil
}
func (b sipSMSBackend) SendCarrierSMS(ctx context.Context, id, number, text, operation, subscription string) (vowifi.SMSSubmitResult, error) {
	s := b.server
	token, err := b.SMSReady(ctx, id)
	if err != nil {
		return vowifi.SMSSubmitResult{}, err
	}
	if token != subscription {
		return vowifi.SMSSubmitResult{}, sipgateway.ErrCarrierUnavailable
	}
	if _, err = device.PrepareSMSSubmitTPDUs(number, text); err != nil {
		return vowifi.SMSSubmitResult{}, errors.Join(sipgateway.ErrSMSInvalid, err)
	}
	state, err := s.vowifi.State(id)
	if err != nil || smsSubscription(state) != subscription {
		return vowifi.SMSSubmitResult{}, sipgateway.ErrCarrierUnavailable
	}
	reservation, err := s.store.ReserveSMSSend(ctx, id, developer.SMSHourlyLimit(ctx, s.store), time.Now().UTC())
	if err != nil {
		return vowifi.SMSSubmitResult{}, err
	}
	if !reservation.Allowed {
		return vowifi.SMSSubmitResult{}, sipgateway.ErrSMSRateLimited
	}
	result, sendErr := s.vowifi.(boundIMSSMSController).SendSMSForSubscription(ctx, id, state.IMSI, state.ICCID, vowifi.SMSSubmitRequest{Recipient: number, Text: text})
	if s.logger != nil {
		s.logger.Info("SIP IMS SMS submission result", "category", "sms", "device_id", id, "operation_id", operation, "parts_attempted", result.PartsAttempted, "parts_accepted", result.PartsAccepted, "delivery_confirmed", result.DeliveryConfirmed)
	}
	// Even a late/uncertain result is stored, without resubmitting through AT.
	// A history write failure must not turn carrier acceptance into a retry.
	if result.PartsAttempted > 0 || result.PartsAccepted > 0 {
		persist, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		config, _ := s.store.Device(persist, id)
		entry, _, _ := s.physicalForConfig(config)
		extra, _ := json.Marshal(map[string]any{"transport": "ims", "gateway": "sip", "parts_total": result.PartsTotal, "parts_attempted": result.PartsAttempted, "parts_accepted": result.PartsAccepted, "all_parts_accepted": result.AllPartsAccepted, "part_results": result.PartResults, "delivery_confirmed": result.DeliveryConfirmed})
		timestamp := result.SubmittedAt
		if timestamp.IsZero() {
			timestamp = time.Now().UTC()
		}
		_, saveErr := s.store.SaveSMSMessage(persist, store.SMSMessage{MessageID: "sip-submit:" + operation, DeviceID: id, ModemIMEI: config.ModemIMEI, ICCID: state.ICCID, IMSI: state.IMSI, LocalPhone: s.smsLocalPhone(persist, id, smsIdentityFromSnapshot(entry.Snapshot), entry.Snapshot), Peer: number, Direction: "outbound", Body: text, Timestamp: timestamp, Status: result.SubmissionStatus, Source: "ims", PartsTotal: result.PartsTotal, DeliveryState: imsSMSDeliveryState(result), Read: true, Extra: extra})
		if saveErr != nil && s.logger != nil {
			s.logger.Warn("SIP SMS history could not be persisted", "category", "sms", "device_id", id)
		}
	}
	return result, sendErr
}

// The cursor survives process restarts; a fresh install starts at Open's SMS
// watermark rather than forwarding the user's historical inbox on enable.
func (s *Server) runSIPSMSSync(ctx context.Context) {
	for ctx.Err() == nil {
		inserted := s.store.SMSInserted()
		if err := s.syncSIPSMSOnce(ctx); err != nil && s.logger != nil {
			s.logger.Debug("SIP SMS forwarding paused", "category", "sms")
		}
		if !waitSMSInserted(ctx, 2*time.Second, inserted) {
			return
		}
	}
}
func (s *Server) syncSIPSMSOnce(ctx context.Context) error {
	if s.sipGateway == nil {
		return nil
	}
	// Retain the public durable cursor for older consumers, but never use it to
	// construct a recipient plan from the configuration observed by this poll.
	latest, err := s.store.LatestSMSMessageID(ctx)
	if err != nil {
		return err
	}
	if err := s.sipGateway.QueueSMS(ctx, latest, nil); err != nil {
		return err
	}
	// Real Inbox saves already captured their first recipient plan and source
	// receipt atomically. A poller must never expand that plan with today's
	// accounts, nor make a deleted/reimported historical row look newly received.
	return s.sipGateway.DeliverSMSOnce(ctx)
}
