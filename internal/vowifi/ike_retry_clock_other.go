//go:build !linux

package vowifi

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Linux is the deployment target. Elsewhere a process-local boot identity
// conservatively rebases stored timers instead of trusting wall-clock jumps.
var ikeRetryProcessStart = time.Now()
var ikeRetryProcessBoot = func() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(value[:])
}()

type systemIKERetryClock struct{}

func (systemIKERetryClock) Snapshot() (IKERetryTime, error) {
	return IKERetryTime{Wall: time.Now().UTC(), Boot: ikeRetryProcessBoot, Uptime: time.Since(ikeRetryProcessStart)}, nil
}
