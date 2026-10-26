package sipgateway

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"vocat/internal/store"
)

// InstallSMSForwarding must run before any live IMS receiver starts. The same
// pure planner can also be installed by Manager.New; it never captures that
// manager's mutable configuration or performs I/O while a Store writer is held.
func InstallSMSForwarding(database *store.Store) {
	if database != nil {
		database.SetSMSForwardPlanner(captureSMSForwarding)
	}
}

type smsCaptureRepository struct{ input store.SMSForwardingInput }

func (r smsCaptureRepository) AppSetting(_ context.Context, key string) (store.AppSetting, error) {
	var value store.AppSetting
	switch key {
	case SettingKey:
		value = r.input.Settings
	case SMSLedgerSettingKey:
		value = r.input.Ledger
	default:
		return value, store.ErrNotFound
	}
	if value.Value == nil {
		return value, store.ErrNotFound
	}
	return value, nil
}
func (smsCaptureRepository) UpsertAppSetting(context.Context, store.AppSetting) error {
	return errors.New("atomic SMS planner cannot write outside its owner transaction")
}

// The settings, actual saved Inbox and ledger all came from one BEGIN IMMEDIATE
// transaction. No new account/permission can be appended by a later poller.
func captureSMSForwarding(input store.SMSForwardingInput) (json.RawMessage, error) {
	if !store.SMSForwardMessageReady(input.Message) || input.Message.Source == "ims-ussd" || input.Settings.Value == nil {
		return nil, nil
	}
	if !input.Settings.Sensitive || len(input.Settings.Value) > store.MaxSIPSettingsBytes {
		return nil, errors.New("atomic SMS recipient settings are not valid private settings")
	}
	var preliminary stored
	if json.Unmarshal(input.Settings.Value, &preliminary) != nil || preliminary.Version != 1 {
		return nil, errors.New("atomic SMS recipient settings are invalid")
	}
	if !preliminary.Config.Enabled || !preliminary.Config.SMSEnabled {
		return nil, nil
	}
	// A pre-epoch database has never durably admitted these recipient identities.
	// Preserve an empty source receipt, not a guessed grant to a future account.
	if preliminary.ConfigRevision == "" {
		return nil, nil
	}
	repository := smsCaptureRepository{input: input}
	config, err := readCurrentConfigAuthority(context.Background(), repository, stored{requireDurableAuthority: true})
	if err != nil {
		return nil, err
	}
	message := IncomingSMS{ID: input.Message.MessageID, DeviceID: input.Message.DeviceID,
		Subscription: input.OriginalICCID, Sender: input.OriginalPeer, Text: input.Message.Body}
	if message.ID == "" {
		message.ID = "row:" + strconv.FormatInt(input.Message.ID, 10)
	}
	in, targeted, err := makeSMSInbox(config, message, input.Now)
	if err != nil || !targeted {
		return nil, err
	}
	in.InboxID, in.SourceKey, in.SourceFingerprint = input.Message.ID, input.SourceKey, input.Fingerprint
	state, err := newSMSService(repository, nil).readLedger(context.Background())
	if err != nil {
		return nil, err
	}
	key := hashCallOperation("sms-source-v1\x00" + input.SourceKey)
	if _, exists := state.Inbox[key]; exists {
		return nil, nil // Independent source receipts also prevent this path.
	}
	if len(state.Inbox) >= 256 {
		return nil, store.ErrSMSForwardCapacity
	}
	state.Inbox[key] = in
	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if len(data) > store.MaxSMSLedgerBytes {
		return nil, store.ErrSMSForwardCapacity
	}
	return data, nil
}

func makeSMSInbox(config stored, message IncomingSMS, now time.Time) (smsInbox, bool, error) {
	reply := ""
	in := smsInbox{Message: message, CanonicalReply: &reply, Until: now.Add(24 * time.Hour), Targets: map[string]smsRecipient{}}
	if !validIncomingSMS(in.Message) {
		in.Rejected, in.Message.Text, in.Message.Sender = true, "", "Unsupported sender"
		if in.Message.ID == "" || len(in.Message.ID) > 512 {
			in.Message.ID = "unsupported-" + hashCallOperation(message.ID)
		}
		if len(in.Message.Subscription) > 256 {
			// Unknown/malformed source must stay read-only; do not turn it into a
			// syntactically valid ICCID or infer the current card.
			in.Message.Subscription = "unknown:" + hashCallOperation(message.Subscription)
		}
	}
	for _, line := range config.Config.CarrierLines {
		if line.DeviceID != message.DeviceID || !line.AllowSMSReceive {
			continue
		}
		if !in.Rejected {
			reply = canonicalSMSReply(line, message.Sender)
		}
		for _, user := range line.SMSRecipients {
			for _, account := range config.Config.Accounts {
				if account.Username != user {
					continue
				}
				identity := recipientIdentity(config, user)
				if identity == "" {
					return in, false, errors.New("SMS local recipient identity unavailable")
				}
				code := 0
				if in.Rejected {
					code = 413
				}
				in.Targets[user] = smsRecipient{Identity: identity, Code: code}
			}
		}
	}
	if user, allowed := peerSMSReceiveTarget(config.Config, message.DeviceID); allowed {
		identity := peerRecipientIdentity(config, user)
		if identity == "" {
			return in, false, errors.New("SMS peer recipient identity unavailable")
		}
		code := 0
		if in.Rejected {
			code = 413
		}
		in.Targets[peerSMSRecipientKey(user)] = smsRecipient{Identity: identity, Code: code, Kind: "peer", User: user}
	}
	if len(in.Targets) > 16 {
		return in, false, store.ErrSMSForwardCapacity
	}
	return in, len(in.Targets) != 0, nil
}

// This runs once at first admission, using the raw sender and the country
// policy from the same receive transaction (or explicit QueueSMS snapshot).
// Empty is an intentional read-only result, never an invitation to retry a
// different country's interpretation in a later listener.
func canonicalSMSReply(line CarrierLine, sender string) string {
	if internationalNumberPattern.MatchString(sender) {
		return sender
	}
	if countryCodePattern.MatchString(line.CountryCode) && line.NationalNumberLength >= 6 &&
		line.NationalNumberLength <= 15-len(line.CountryCode) && plainNumberPattern.MatchString(sender) && len(sender) == line.NationalNumberLength {
		reply := "+" + line.CountryCode + sender
		if internationalNumberPattern.MatchString(reply) {
			return reply
		}
	}
	return ""
}
