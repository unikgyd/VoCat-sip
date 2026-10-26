package sipgateway

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
)

// Routes contain no built-in number, country, module, or external account.
// A default line does not implicitly grant any extension permission to use it.
type CarrierLine struct {
	DeviceID             string   `json:"device_id"`
	Label                string   `json:"label"`
	Prefix               string   `json:"prefix"`
	Default              bool     `json:"default"`
	AllowOutgoing        bool     `json:"allow_outgoing"`
	AllowIncoming        bool     `json:"allow_incoming"`
	IncomingExtensions   []string `json:"incoming_extensions"`
	CountryCode          string   `json:"country_code"`
	NationalNumberLength int      `json:"national_number_length"`
	AllowSMSSend         bool     `json:"allow_sms_send"`
	AllowSMSReceive      bool     `json:"allow_sms_receive"`
	SMSRecipients        []string `json:"sms_recipients"`
}

var deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var routePrefixPattern = regexp.MustCompile(`^[0-9]{1,8}$`)
var countryCodePattern = regexp.MustCompile(`^[1-9][0-9]{0,2}$`)
var internationalNumberPattern = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
var plainNumberPattern = regexp.MustCompile(`^[0-9]+$`)

func prepareCarrierConfig(config *Config) error {
	if len(config.CarrierLines) > 32 {
		return errors.New("SIP supports at most 32 carrier lines")
	}
	config.CarrierLines = append([]CarrierLine(nil), config.CarrierLines...)
	ids, prefixes := map[string]bool{}, map[string]bool{}
	defaults := 0
	accounts := map[string]bool{}
	for _, account := range config.Accounts {
		accounts[account.Username] = true
	}
	for i, line := range config.CarrierLines {
		if !deviceIDPattern.MatchString(line.DeviceID) || ids[line.DeviceID] || len(line.Label) > 80 || strings.ContainsFunc(line.Label, unicode.IsControl) {
			return errors.New("SIP carrier line requires a unique configured device ID and a safe label")
		}
		ids[line.DeviceID] = true
		if len(line.IncomingExtensions) > 16 {
			return errors.New("A carrier line supports at most 16 incoming extensions")
		}
		seenTargets := map[string]bool{}
		for _, user := range line.IncomingExtensions {
			if !accounts[user] || seenTargets[user] {
				return errors.New("Incoming carrier targets must be unique configured extensions")
			}
			seenTargets[user] = true
		}
		config.CarrierLines[i].IncomingExtensions = append([]string(nil), line.IncomingExtensions...)
		if len(line.SMSRecipients) > 16 {
			return errors.New("A carrier line supports at most 16 SMS recipients")
		}
		seenSMS := map[string]bool{}
		for _, user := range line.SMSRecipients {
			if !accounts[user] || seenSMS[user] {
				return errors.New("SMS recipients must be unique configured extensions")
			}
			seenSMS[user] = true
		}
		config.CarrierLines[i].SMSRecipients = append([]string(nil), line.SMSRecipients...)
		if line.Prefix != "" {
			if !routePrefixPattern.MatchString(line.Prefix) || prefixes[line.Prefix] {
				return errors.New("SIP carrier prefixes must be unique 1..8 digit strings")
			}
			prefixes[line.Prefix] = true
		}
		if line.Default {
			defaults++
		}
		if line.CountryCode == "" {
			if line.NationalNumberLength != 0 {
				return errors.New("A national number length requires an explicit country code")
			}
		} else if !countryCodePattern.MatchString(line.CountryCode) || line.NationalNumberLength < 6 || line.NationalNumberLength > 15-len(line.CountryCode) {
			return errors.New("SIP country code or national number length is invalid")
		}
	}
	if defaults > 1 {
		return errors.New("SIP permits only one default carrier line")
	}
	if config.CarrierEnabled && len(ids) == 0 {
		return errors.New("Add a carrier line before enabling external calls")
	}
	if config.SMSEnabled && len(ids) == 0 {
		return errors.New("Add a carrier line before enabling SMS forwarding")
	}
	for i, account := range config.Accounts {
		if len(account.OutgoingLines) > 32 {
			return errors.New("Too many permitted carrier lines")
		}
		seen := map[string]bool{}
		for _, id := range account.OutgoingLines {
			if !ids[id] || seen[id] {
				return errors.New("SIP account carrier permissions must reference unique configured lines")
			}
			seen[id] = true
		}
		config.Accounts[i].OutgoingLines = append([]string(nil), account.OutgoingLines...)
		if len(account.SMSSendLines) > 32 {
			return errors.New("Too many permitted SMS lines")
		}
		seenSMS := map[string]bool{}
		for _, id := range account.SMSSendLines {
			if !ids[id] || seenSMS[id] {
				return errors.New("SMS send permissions must reference unique configured lines")
			}
			seenSMS[id] = true
		}
		config.Accounts[i].SMSSendLines = append([]string(nil), account.SMSSendLines...)
	}
	return nil
}

func cloneConfig(config Config) Config {
	config.Accounts = append([]Account{}, config.Accounts...)
	config.AllowedCIDRs = append([]string{}, config.AllowedCIDRs...)
	config.CarrierLines = append([]CarrierLine{}, config.CarrierLines...)
	for i := range config.CarrierLines {
		config.CarrierLines[i].IncomingExtensions = append([]string{}, config.CarrierLines[i].IncomingExtensions...)
		config.CarrierLines[i].SMSRecipients = append([]string{}, config.CarrierLines[i].SMSRecipients...)
	}
	for i := range config.Accounts {
		config.Accounts[i].OutgoingLines = append([]string{}, config.Accounts[i].OutgoingLines...)
		config.Accounts[i].SMSSendLines = append([]string{}, config.Accounts[i].SMSSendLines...)
	}
	config.Peer = clonePeerConfig(config.Peer)
	return config
}

func carrierRoute(config Config, account Account, dialed string) (CarrierLine, string, int) {
	if !config.CarrierEnabled {
		return CarrierLine{}, "", 404
	}
	var selected *CarrierLine
	for i := range config.CarrierLines {
		line := &config.CarrierLines[i]
		if line.Prefix != "" && strings.HasPrefix(dialed, line.Prefix) && (selected == nil || len(line.Prefix) > len(selected.Prefix)) {
			selected = line
		}
	}
	if selected != nil {
		dialed = strings.TrimPrefix(dialed, selected.Prefix)
	} else {
		for i := range config.CarrierLines {
			if config.CarrierLines[i].Default {
				selected = &config.CarrierLines[i]
				break
			}
		}
	}
	if selected == nil {
		return CarrierLine{}, "", 404
	}
	permitted := false
	for _, id := range account.OutgoingLines {
		if id == selected.DeviceID {
			permitted = true
		}
	}
	if !account.AllowCalls || !selected.AllowOutgoing || !permitted {
		return CarrierLine{}, "", 403
	}
	if !strings.HasPrefix(dialed, "+") && selected.CountryCode != "" && len(dialed) == selected.NationalNumberLength && plainNumberPattern.MatchString(dialed) {
		dialed = "+" + selected.CountryCode + dialed
	}
	if !internationalNumberPattern.MatchString(dialed) {
		return CarrierLine{}, "", 484
	}
	return *selected, dialed, 0
}
