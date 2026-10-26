package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

func TestEPDGStoreScopesHistoryToCardAndIdentityWithoutRawIdentifiers(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	s := EPDGStore{Store: db}
	id := vowifi.SIMIdentity{ICCID: "8944100000000000001", IMSI: "234150000000001", HomeMCC: "234", HomeMNC: "15"}
	if _, err := s.SetConfig(ctx, id.ICCID, vowifi.EPDGDiscoveryOptions{PreferredAddresses: []string{"manual.example"}, AdditionalPLMNs: []string{"515-66"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RememberRegistered(ctx, id, vowifi.EPDGCandidate{Host: "worked.example"}); err != nil {
		t.Fatal(err)
	}
	setting, err := db.AppSetting(ctx, EPDGSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	if !setting.Sensitive || strings.Contains(string(setting.Value), id.ICCID) || strings.Contains(string(setting.Value), id.IMSI) || string(setting.Public().Value) != "null" {
		t.Fatal("history exposed SIM identities")
	}
	options, err := s.LoadDiscovery(ctx, id)
	if err != nil || options.LastSuccessful == nil || options.LastSuccessful.Host != "worked.example" || options.AdditionalPLMNs[0] != "51566" {
		t.Fatalf("history/config %v %v", options, err)
	}
	for _, other := range []vowifi.SIMIdentity{{ICCID: id.ICCID, IMSI: "234150000000002", HomeMCC: "234", HomeMNC: "15"}, {ICCID: "8944100000000000002", IMSI: id.IMSI, HomeMCC: "234", HomeMNC: "15"}, {ICCID: id.ICCID, IMSI: id.IMSI, HomeMCC: "234", HomeMNC: "30"}} {
		options, err := s.LoadDiscovery(ctx, other)
		if err != nil || options.LastSuccessful != nil {
			t.Fatal("history crossed card, identity or authentication home")
		}
	}
	if _, err := s.SetConfig(ctx, id.ICCID, vowifi.EPDGDiscoveryOptions{}); err != nil {
		t.Fatal(err)
	}
	options, _ = s.LoadDiscovery(ctx, id)
	if len(options.PreferredAddresses) != 0 || options.LastSuccessful == nil {
		t.Fatal("restore auto removed successful history")
	}
}
func TestEPDGStoreRejectsInvalidManualPolicyAndBoundsHistory(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	s := EPDGStore{Store: db}
	if _, err := s.SetConfig(ctx, "card", vowifi.EPDGDiscoveryOptions{ManualOnly: true}); err == nil {
		t.Fatal("invalid manual policy accepted")
	}
	for i := 0; i < 130; i++ {
		id := vowifi.SIMIdentity{ICCID: fmt.Sprint(894410000 + i), IMSI: "234150000000000", HomeMCC: "234", HomeMNC: "15"}
		if err := s.RememberRegistered(ctx, id, vowifi.EPDGCandidate{Host: "worked.example"}); err != nil {
			t.Fatal(err)
		}
	}
	setting, err := db.AppSetting(ctx, EPDGSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	var doc epdgDocument
	if json.Unmarshal(setting.Value, &doc) != nil || len(doc.History) != 128 {
		t.Fatal("unbounded success history")
	}
}
func TestEPDGStoreIgnoresMalformedHistoryWithoutBypassingManualPolicy(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	s := EPDGStore{Store: db}
	id := vowifi.SIMIdentity{ICCID: "8944100000000000001", IMSI: "234150000000000", HomeMCC: "234", HomeMNC: "15"}
	if _, err := s.SetConfig(ctx, id.ICCID, vowifi.EPDGDiscoveryOptions{PreferredAddresses: []string{"manual.example"}, ManualOnly: true, GeographicDNSFallback: true}); err != nil {
		t.Fatal(err)
	}
	setting, err := db.AppSetting(ctx, EPDGSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	json.Unmarshal(setting.Value, &wire)
	wire["history"] = map[string]any{epdgHash(id.ICCID + "\n" + id.IMSI): map[string]any{"home_mnc": 123, "host": "invalid.example"}}
	data, _ := json.Marshal(wire)
	if err := db.UpsertAppSetting(ctx, store.AppSetting{Key: EPDGSettingKey, Value: data, Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	options, err := s.LoadDiscovery(ctx, id)
	if err != nil || options.LastSuccessful != nil || !options.ManualOnly || !options.GeographicDNSFallback || len(options.PreferredAddresses) != 1 {
		t.Fatalf("malformed optional history changed policy: %+v %v", options, err)
	}
}
func TestEPDGStoreNeverSilentlyBypassesCorruptManualConfiguration(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	db.UpsertAppSetting(ctx, store.AppSetting{Key: EPDGSettingKey, Value: json.RawMessage(`{"version":99}`), Sensitive: true})
	_, err := (EPDGStore{Store: db}).LoadDiscovery(ctx, vowifi.SIMIdentity{ICCID: "card"})
	if err == nil {
		t.Fatal("unknown private config silently became automatic discovery")
	}
}
