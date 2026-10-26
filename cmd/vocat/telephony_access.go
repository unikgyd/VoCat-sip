package main

import (
	"context"
	"errors"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

var errTelephonySubscriptionChanged = errors.New("telephony: active subscription or access policy changed")

type subscriptionSIMReader struct {
	reader   vowifi.SIMIdentityReader
	database *store.Store
}

func (s subscriptionSIMReader) ReadIdentity(ctx context.Context, id string) (vowifi.SIMIdentity, error) {
	before, err := s.database.Device(ctx, id)
	if err != nil {
		return vowifi.SIMIdentity{}, err
	}
	identity, err := s.reader.ReadIdentity(ctx, id)
	if err != nil {
		return vowifi.SIMIdentity{}, err
	}
	after, err := s.database.Device(ctx, id)
	if err != nil {
		return vowifi.SIMIdentity{}, err
	}
	if err := ctx.Err(); err != nil {
		return vowifi.SIMIdentity{}, err
	}
	if !store.SameSubscription(before, after) || before.Telephony != nil && identity.ICCID != before.Telephony.ICCID {
		return vowifi.SIMIdentity{}, errTelephonySubscriptionChanged
	}
	return identity, nil
}

type subscriptionSMSReader struct {
	subscriptionSIMReader
	center vowifi.SMSCenterReader
}

func (s subscriptionSMSReader) ReadSMSCenter(ctx context.Context, id string) (string, error) {
	return s.center.ReadSMSCenter(ctx, id)
}

func guardedSubscriptionReader(database *store.Store, adapter vowifiDeviceAdapter) vowifi.SIMIdentityReader {
	guard := subscriptionSIMReader{reader: adapter, database: database}
	if center, ok := adapter.(vowifi.SMSCenterReader); ok {
		return subscriptionSMSReader{guard, center}
	}
	return guard
}

func applicationWiFiRadioPolicy(ctx context.Context, database *store.Store, id string) (vowifi.RadioPolicy, error) {
	config, err := database.Device(ctx, id)
	if err != nil {
		return "", err
	}
	if config.Telephony == nil {
		return vowifi.RadioPolicyExclusive, nil
	}
	if !store.AccessEnabled(config, store.IMSModeVoWiFi) {
		return "", errTelephonySubscriptionChanged
	}
	return vowifi.RadioPolicy(config.Telephony.WiFiRadioPolicy), nil
}
