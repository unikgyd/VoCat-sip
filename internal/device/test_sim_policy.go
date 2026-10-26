package device

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
)

type testSIMPolicy struct {
	IMSIs map[string]struct{}
}

var configuredTestSIMs atomic.Pointer[testSIMPolicy]

// LoadAuthorizedTestSIMs installs the operator's explicit, exact-subscriber
// test policy before hardware discovery. It is not a grant of software or
// carrier authorization. No prefix, wildcard, MCC-wide or missing-identity
// exception is accepted. The file is read once, so edits require a restart.
func LoadAuthorizedTestSIMs(path string) error {
	if strings.TrimSpace(path) == "" {
		configuredTestSIMs.Store(nil)
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open authorized test SIM policy: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return errors.New("authorized test SIM policy must be a regular file no larger than 64 KiB")
	}
	decoder := json.NewDecoder(io.LimitReader(f, (64<<10)+1))
	decoder.DisallowUnknownFields()
	var document struct {
		IMSIs []string `json:"authorized_test_imsis"`
	}
	if err := decoder.Decode(&document); err != nil {
		return errors.New("invalid authorized test SIM policy JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("authorized test SIM policy must contain one JSON object")
	}
	if len(document.IMSIs) > 10 {
		return errors.New("authorized test SIM policy supports at most 10 exact subscribers")
	}
	policy := &testSIMPolicy{IMSIs: make(map[string]struct{}, len(document.IMSIs))}
	for _, imsi := range document.IMSIs {
		if len(imsi) < 14 || len(imsi) > 16 || strings.Trim(imsi, "0123456789") != "" || IsPlaceholderIMSI(imsi) {
			return errors.New("authorized test SIM policy requires complete numeric subscriber identities")
		}
		if _, exists := policy.IMSIs[imsi]; exists {
			return errors.New("authorized test SIM policy contains a duplicate subscriber")
		}
		policy.IMSIs[imsi] = struct{}{}
	}
	configuredTestSIMs.Store(policy)
	return nil
}

func authorizedTestSIM(imsi string) bool {
	policy := configuredTestSIMs.Load()
	if policy == nil {
		return false
	}
	_, ok := policy.IMSIs[imsi]
	return ok
}
