package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

// A task keeps its original call manager and call ID until release is proven.
// It does not schedule an unowned timer and restore the SIM while still dialing.
func (s *Server) executeOwnedAutomaticCall(ctx context.Context, config store.Device, physical string, task store.AutomaticTask, payload automaticTaskPayload) (output string, err error) {
	if !validDialNumber(payload.Phone) || payload.DurationSeconds < 1 || payload.DurationSeconds > 600 {
		return "", automaticTaskExecutionError{err: errors.New("invalid automatic call number or duration"), retryable: false}
	}
	if _, current, present := s.physicalForConfig(config); !present || current != physical {
		return "", errors.New("automatic call physical device changed")
	}
	var controller VoWiFiCallController
	var call vowifi.Call
	var dialErr error
	if s.callTransportForConfig(config) == "cellular" {
		if s.moduleCalls == nil {
			return "", errors.New("the device provider does not expose SIM-bound signaling calls")
		}
		identity, observeErr := s.observedTelephonySubscription(ctx, config)
		if observeErr != nil || identity.ICCID != task.ProfileICCID {
			return "", errors.New("automatic call subscription could not be verified")
		}
		controller = s.moduleCalls
		call, dialErr = s.moduleCalls.DialSignalingCall(ctx, config.ID, payload.Phone)
		if call.ID == "" && dialErr != nil {
			return "", dialErr // the module manager returns an ID after any attempted ATD
		}
	} else {
		if readyErr := (sipCarrierBackend{s}).Ready(ctx, config.ID); readyErr != nil {
			return "", readyErr
		}
		var ok bool
		controller, ok = s.vowifi.(VoWiFiCallController)
		if !ok {
			return "", errIMSUnavailable
		}
		call, dialErr = controller.DialCall(ctx, config.ID, payload.Phone)
	}
	if call.ID == "" {
		// An absent ID is not proof that signaling was never submitted.
		return "", automaticTaskExecutionError{err: errors.Join(dialErr, errors.New("automatic call did not return its original call ID")), retryable: false}
	}
	ended := func() (bool, error) {
		calls, err := controller.Calls(config.ID)
		if err != nil {
			return false, err
		}
		for _, current := range calls {
			if current.ID == call.ID {
				return current.EndedAt != nil && !current.SignalingPending, nil
			}
		}
		return false, errors.New("original automatic call state is unavailable")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if done, observeErr := ended(); observeErr == nil && done {
			return
		}
		hangupErr := controller.HangupCall(cleanup, config.ID, call.ID)
		for {
			if done, observeErr := ended(); observeErr == nil && done {
				return
			}
			select {
			case <-cleanup.Done():
				output = ""
				err = automaticTaskExecutionError{err: errors.Join(err, hangupErr, errors.New("original automatic call cleanup remains unconfirmed")), retryable: false}
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	if dialErr != nil {
		return "", automaticTaskExecutionError{err: dialErr, retryable: false}
	}
	timer := time.NewTimer(time.Duration(payload.DurationSeconds) * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", automaticTaskExecutionError{err: ctx.Err(), retryable: false}
		case <-timer.C:
			return fmt.Sprintf("已拨打 %s，限时 %d 秒，呼叫已结束", payload.Phone, payload.DurationSeconds), nil
		case <-ticker.C:
			done, observeErr := ended()
			if observeErr != nil {
				return "", automaticTaskExecutionError{err: observeErr, retryable: false}
			}
			if done {
				return "呼叫已结束：" + payload.Phone, nil
			}
		}
	}
}
