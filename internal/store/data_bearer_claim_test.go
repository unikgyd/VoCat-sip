package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestDataBearerVersionTwoFencesOnlyItsEquipmentAndPhysicalIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "claims.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	equipment, physical, other := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	raw, _ := json.Marshal(map[string]any{"Version": 2, "EquipmentKey": equipment, "PhysicalKey": physical, "Host": map[string]any{"Ready": true}})
	if ok, err := db.CompareDataBearerClaim(ctx, DataBearerClaimPrefix+equipment, nil, raw); !ok || err != nil {
		t.Fatal(err)
	}
	for _, scope := range []struct {
		equipment, physical string
		pending             bool
	}{{equipment, "", true}, {"", physical, true}, {other, "", false}, {"", other, false}} {
		if pending, err := db.HasDataBearerClaim(ctx, scope.equipment, scope.physical); err != nil || pending != scope.pending {
			t.Fatal(scope, pending, err)
		}
	}
	if err := db.UpsertAppSetting(ctx, AppSetting{Key: "unrelated.secret", Value: json.RawMessage(`"must-not-be-enumerated"`), Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	claims, err := db.DataBearerClaims(ctx)
	if err != nil || len(claims) != 1 || claims[0].Key != DataBearerClaimPrefix+equipment || !claims[0].Sensitive {
		t.Fatal("journal reader leaked unrelated settings", err)
	}
}
