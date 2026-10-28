package device

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuthorizedTestSIMPolicyIsExactAndRevocable(t *testing.T) {
	BlockedMCCs["460"] = "中国"
	BlockedMCCs["461"] = "中国"
	t.Cleanup(func() { delete(BlockedMCCs, "460"); delete(BlockedMCCs, "461") })
	previous := configuredTestSIMs.Load()
	t.Cleanup(func() { configuredTestSIMs.Store(previous) })
	path := filepath.Join(t.TempDir(), "test-sims.json")
	if err := os.WriteFile(path, []byte(`{"authorized_test_imsis":["460119876543210"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadAuthorizedTestSIMs(path); err != nil {
		t.Fatal(err)
	}
	if RegionBlockReason("460119876543210") != "" {
		t.Fatal("explicit subscriber was blocked")
	}
	for _, other := range []string{"460119876543211", "460001234567890", "461001234567890"} {
		if RegionBlockReason(other) == "" {
			t.Fatal("unlisted subscriber was allowed")
		}
	}
	if err := LoadAuthorizedTestSIMs(""); err != nil {
		t.Fatal(err)
	}
	if RegionBlockReason("460119876543210") == "" {
		t.Fatal("removing the explicit file did not revoke the exception")
	}
}

func TestAuthorizedTestSIMPolicyRejectsBroadAndMalformedInputs(t *testing.T) {
	BlockedMCCs["460"] = "中国"
	BlockedMCCs["461"] = "中国"
	t.Cleanup(func() { delete(BlockedMCCs, "460"); delete(BlockedMCCs, "461") })
	previous := configuredTestSIMs.Load()
	t.Cleanup(func() { configuredTestSIMs.Store(previous) })
	path := filepath.Join(t.TempDir(), "test-sims.json")
	for _, raw := range []string{
		`{"authorized_test_imsis":["460"]}`,
		`{"authorized_test_imsis":["46011*"]}`,
		`{"authorized_test_imsis":["460000000000000"]}`,
		`{"authorized_test_imsis":["460119876543210","460119876543210"]}`,
		`{"authorized_test_imsis":["460119876543210"],"allow_all":true}`,
		`{"authorized_test_imsis":["460119876543210"]} {}`,
	} {
		configuredTestSIMs.Store(nil)
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := LoadAuthorizedTestSIMs(path); err == nil {
			t.Fatal("invalid policy was accepted")
		}
		if RegionBlockReason("460119876543210") == "" {
			t.Fatal("invalid policy changed admission")
		}
	}
	if err := LoadAuthorizedTestSIMs(path + "-missing"); err == nil {
		t.Fatal("missing explicit file was accepted")
	}
}
