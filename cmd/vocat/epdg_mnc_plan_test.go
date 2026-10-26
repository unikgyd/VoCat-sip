package main

import (
	"context"
	"testing"

	"vocat/internal/store"
)

func TestMNCPlanApplicationEnablesRealmAwareReaderWithoutCellularData(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	options := ec20VoWiFiAdapterOptions(db)
	if !options.AllowUnresolvedMNCLength || options.RestoreCellularData || options.PureAirplanePolicy == nil || options.PureAirplanePolicy("missing") {
		t.Fatal("production reader defaults changed")
	}
}
