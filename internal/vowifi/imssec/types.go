// Package imssec describes portable 3GPP IMS transport-mode security plans.
// It has no dependency on IMS sessions, device discovery, a bearer owner or
// Linux commands. A plan describes desired objects, never kernel ownership.
package imssec

import (
	"context"
	"fmt"
	"io"
	"net"
)

// Config is the complete evidence-derived four-SA agreement. UE SPIs identify
// inbound SAs; P-CSCF SPIs identify outbound SAs. Installers must not retain
// EncryptionKey or IntegrityKey after Install returns, including on failure.
type Config struct {
	LocalIP, RemoteIP                                            net.IP
	IntegrityAlgorithm                                           string // hmac-md5-96 or hmac-sha-1-96; empty defaults to SHA1
	EncryptionAlgorithm                                          string // null, des-ede3-cbc or aes-cbc; empty defaults to AES
	UEClientSPI, UEServerSPI, PCSCFClientSPI, PCSCFServerSPI     uint32
	UEClientPort, UEServerPort, PCSCFClientPort, PCSCFServerPort int
	EncryptionKey, IntegrityKey                                  []byte
}

func (Config) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private IMS security agreement>")
}

type Handle interface {
	Close(context.Context) error
}

type Installer interface {
	Install(context.Context, Config) (Handle, error)
}

// CloneConfig detaches every mutable input. Callers must erase the copied key
// slices when finished. BuildPlan does not copy or retain any key material.
func CloneConfig(config Config) Config {
	config.LocalIP = append(net.IP(nil), config.LocalIP...)
	config.RemoteIP = append(net.IP(nil), config.RemoteIP...)
	config.EncryptionKey = append([]byte(nil), config.EncryptionKey...)
	config.IntegrityKey = append([]byte(nil), config.IntegrityKey...)
	return config
}
