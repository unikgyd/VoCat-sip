package server

import (
	"encoding/json"
	"strings"
	"time"

	"vocat/internal/device"
	"vocat/internal/vowifi"
)

// This is a display observation, never an admission check. Deliberately omit
// NotBefore: a wall-clock deadline must not masquerade as permission to send.
func addIKERetryObservation(result map[string]any, status *vowifi.IKERetryStatus, at time.Time, cached, cleanup bool) {
	if status == nil {
		return
	}
	value := *status
	validSource := value.Source == "operator" || value.Source == "implementation-missing" || value.Source == "implementation-invalid" ||
		value.Source == "uncertain-attempt" || value.Source == "store-unavailable" || value.Source == "binding-unavailable"
	validScope := value.Scope == "" || value.Scope == string(vowifi.EPDGRejectionEndpoint) ||
		value.Scope == string(vowifi.EPDGRejectionPLMN) || value.Scope == string(vowifi.EPDGRejectionPLMNAPN) ||
		value.Scope == "unbound_plmn" || value.Scope == "unbound_plmn_apn"
	if !validSource || !validScope || value.AfterSeconds < 0 || value.AfterSeconds > 1116000 {
		value = vowifi.IKERetryStatus{Source: "status-unavailable", UntilCardEvent: true}
	}
	observation := map[string]any{
		"source": value.Source, "scope": value.Scope, "notify_type": value.NotifyType, "after_seconds": value.AfterSeconds,
		"until_card_event": value.UntilCardEvent, "cached": cached, "cleanup_pending": cleanup,
	}
	if !at.IsZero() {
		observation["observed_at"] = at.UTC()
	}
	result["ike_retry"] = observation
}

func addStoredIKERetryObservation(result map[string]any, extra json.RawMessage, at time.Time) {
	var outer struct {
		Retry   json.RawMessage `json:"ike_retry"`
		Cleanup []string        `json:"cleanup_errors"`
	}
	if json.Unmarshal(extra, &outer) != nil || len(outer.Retry) == 0 || string(outer.Retry) == "null" {
		return
	}
	var status vowifi.IKERetryStatus
	if len(outer.Retry) > 4096 || json.Unmarshal(outer.Retry, &status) != nil {
		status = vowifi.IKERetryStatus{Source: "status-unavailable", UntilCardEvent: true}
	}
	// Typed extraction and an explicit output list keep arbitrary Extra fields,
	// private journal keys, subscriber identities and attempt tokens out of this object.
	addIKERetryObservation(result, &status, at, true, len(outer.Cleanup) > 0)
}

func bindIKERetryObservation(result map[string]any, configuredID string, entry *device.Device) {
	if result["ike_retry"] == nil {
		return
	}
	current := strings.TrimSpace(snapshotString(snapshotForEntry(entry), func(snapshot *device.Snapshot) string { return snapshot.ICCID }))
	runtimeCard, _ := result["iccid"].(string)
	runtimeDevice, _ := result["device_id"].(string)
	if current == "" || strings.TrimSpace(runtimeCard) != current || runtimeDevice != configuredID {
		delete(result, "ike_retry")
	}
}
