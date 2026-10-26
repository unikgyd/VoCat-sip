package integration

import (
	"context"
	"path/filepath"
	"testing"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

func TestMNCPlanHistorySurvivesDatabaseReopenAndKeepsAuthority(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "epdg.sqlite")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	id := vowifi.SIMIdentity{ICCID: "8901000000000000001", IMSI: "999123123456789", HomeMCC: "999", HomeMNC: "123", HomeMNCUncertain: true}
	if err = (EPDGStore{Store: db}).RememberRegistered(ctx, id, vowifi.EPDGCandidate{Host: "worked.example"}); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id.HomeMNC = "12"
	options, err := (EPDGStore{Store: db}).LoadDiscovery(ctx, id)
	if err != nil || options.LastSuccessful == nil {
		t.Fatalf("inferred success was lost: %+v %v", options, err)
	}
	plans, err := vowifi.BuildEPDGAttempts(id, options)
	if err != nil || plans[0].Candidate.Host != "worked.example" || plans[0].Identity.HomeMNC != "123" {
		t.Fatalf("wrong remembered pair: %+v %v", plans, err)
	}
	for _, other := range []vowifi.SIMIdentity{
		{ICCID: id.ICCID, IMSI: id.IMSI, HomeMCC: "999", HomeMNC: "12"},
		{ICCID: id.ICCID, IMSI: "999123123456788", HomeMCC: "999", HomeMNC: "12", HomeMNCUncertain: true},
		{ICCID: "8901000000000000002", IMSI: id.IMSI, HomeMCC: "999", HomeMNC: "12", HomeMNCUncertain: true},
	} {
		got, err := (EPDGStore{Store: db}).LoadDiscovery(ctx, other)
		if err != nil || got.LastSuccessful != nil {
			t.Fatal("history crossed known EF_AD, SIM or IMSI")
		}
	}
}
