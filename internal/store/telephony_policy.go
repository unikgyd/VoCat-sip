package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TelephonyPolicy belongs to a subscription, not a USB port. The device row
// holds the currently applied subscription; the SIM table survives moving a
// card to another configured modem. DefaultRoute affects only new operations.
type TelephonyPolicy struct {
	ICCID               string `json:"iccid"`
	CellularMode        string `json:"cellular_mode"`
	AutoConnectCellular bool   `json:"auto_connect_cellular"`
	AutoConnectWiFi     bool   `json:"auto_connect_wifi"`
	DefaultRoute        string `json:"default_route"`
	WiFiRadioPolicy     string `json:"wifi_radio_policy"`
}

func (p TelephonyPolicy) Validate() error {
	if len(p.ICCID) < 10 || len(p.ICCID) > 22 || strings.Trim(p.ICCID, "0123456789") != "" {
		return errors.New("telephony policy requires the live SIM ICCID")
	}
	if p.CellularMode != IMSModeModule && p.CellularMode != IMSModeHost {
		return errors.New("cellular mode must be module or host_ims")
	}
	if p.DefaultRoute != IMSModeModule && p.DefaultRoute != IMSModeHost && p.DefaultRoute != IMSModeVoWiFi {
		return errors.New("default route must be module, host_ims or vowifi")
	}
	if p.WiFiRadioPolicy != "coexist" && p.WiFiRadioPolicy != "exclusive" {
		return errors.New("Wi-Fi radio policy must be coexist or exclusive")
	}
	if p.AutoConnectCellular && p.WiFiRadioPolicy == "exclusive" {
		return errors.New("cellular auto-connect requires the coexist Wi-Fi radio policy")
	}
	return nil
}

func CellularMode(value Device) string {
	if value.Telephony != nil {
		return value.Telephony.CellularMode
	}
	if value.IMSMode == IMSModeHost {
		return IMSModeHost
	}
	return IMSModeModule
}

func AccessEnabled(value Device, mode string) bool {
	if mode != IMSModeVoWiFi && NormalizeDeviceType(value.DeviceType) == DeviceTypeUSBSIMReader {
		return false
	}
	if p := value.Telephony; p != nil {
		switch mode {
		case IMSModeVoWiFi:
			return p.AutoConnectWiFi
		case IMSModeModule, IMSModeHost:
			return p.CellularMode == mode && p.AutoConnectCellular
		default:
			return false
		}
	}
	if mode == IMSModeModule {
		return NormalizeDeviceType(value.DeviceType) != DeviceTypeUSBSIMReader && EffectiveIMSMode(value) == IMSModeModule
	}
	return EffectiveIMSMode(value) == mode && EffectiveIMSEnabled(value)
}

func WiFiExclusive(value Device) bool {
	return value.Telephony == nil || value.Telephony.WiFiRadioPolicy == "exclusive"
}

// HostIMSConfigured is hardware ownership, independent of the default route
// and current registration. A disabled Host IMS configuration can still have
// pending durable cleanup and must not be borrowed by a legacy QMI path.
func HostIMSConfigured(value Device) bool {
	return NormalizeDeviceType(value.DeviceType) != DeviceTypeUSBSIMReader && CellularMode(value) == IMSModeHost
}

func SameSubscription(a, b Device) bool {
	if a.Telephony == nil || b.Telephony == nil {
		return a.Telephony == nil && b.Telephony == nil
	}
	return a.Telephony.ICCID == b.Telephony.ICCID
}

func LegacyTelephonyPolicy(value Device, iccid string) TelephonyPolicy {
	p := TelephonyPolicy{ICCID: iccid, CellularMode: CellularMode(value), DefaultRoute: EffectiveIMSMode(value),
		AutoConnectCellular: AccessEnabled(value, CellularMode(value)), AutoConnectWiFi: AccessEnabled(value, IMSModeVoWiFi), WiFiRadioPolicy: "coexist"}
	if EffectiveIMSMode(value) == IMSModeVoWiFi {
		p.WiFiRadioPolicy = "exclusive"
	}
	return p
}

func (s *Store) SIMTelephonyPolicy(ctx context.Context, iccid string) (TelephonyPolicy, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT policy_json FROM sim_telephony_policies WHERE iccid=?`, iccid).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TelephonyPolicy{}, ErrNotFound
		}
		return TelephonyPolicy{}, err
	}
	var policy TelephonyPolicy
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return policy, err
	}
	if policy.ICCID != iccid {
		return policy, errors.New("stored telephony subscription mismatch")
	}
	return policy, policy.Validate()
}

// TelephonyForCard restores only this ICCID's saved intent. A replacement SIM
// never inherits the previous SIM's cellular mode or auto-connect flags.
func (s *Store) TelephonyForCard(ctx context.Context, previous Device, card CardPolicy) (TelephonyPolicy, error) {
	p, err := s.SIMTelephonyPolicy(ctx, card.ICCID)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return p, err
	}
	if previous.Telephony == nil {
		p = LegacyTelephonyPolicy(previous, card.ICCID)
		if p.CellularMode == IMSModeModule && card.AirplaneEnabled {
			p.AutoConnectCellular = false
		}
	} else {
		p = TelephonyPolicy{ICCID: card.ICCID, CellularMode: IMSModeModule, DefaultRoute: IMSModeModule,
			AutoConnectCellular: !card.AirplaneEnabled && !card.VoWiFiEnabled, AutoConnectWiFi: card.VoWiFiEnabled, WiFiRadioPolicy: "coexist"}
		if card.VoWiFiEnabled {
			p.DefaultRoute, p.WiFiRadioPolicy = IMSModeVoWiFi, "exclusive"
		}
	}
	return p, p.Validate()
}

// SaveTelephonyPolicy updates the per-SIM preferences and active projection in
// one transaction. Callers must hold the line lifecycle fence and validate its
// live hardware/SIM identity before and after stopping affected access owners.
func (s *Store) SaveTelephonyPolicy(ctx context.Context, config Device, policy TelephonyPolicy, cards ...CardPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if len(cards) > 1 || len(cards) == 1 && cards[0].ICCID != policy.ICCID {
		return errors.New("telephony card projection subscription mismatch")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	config.Telephony = &policy
	if err := upsertDevice(ctx, tx, config); err != nil {
		return err
	}
	if len(cards) == 1 {
		if err := upsertCardPolicy(ctx, tx, cards[0]); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sim_telephony_policies(iccid,policy_json,updated_at) VALUES(?,?,?)
		ON CONFLICT(iccid) DO UPDATE SET policy_json=excluded.policy_json,updated_at=excluded.updated_at`, policy.ICCID, string(raw), time.Now().Unix()); err != nil {
		return fmt.Errorf("save SIM telephony policy: %w", err)
	}
	return tx.Commit()
}
