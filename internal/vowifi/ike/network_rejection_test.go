package ike

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

func TestBackoffEvidenceSharedTw3Vectors(t *testing.T) {
	raw, err := os.ReadFile("../../../../contracts/ike-backoff-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Version int
		Vectors []struct {
			Name, DataHex, Status string
			Seconds               *int64
		}
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Version != 1 || len(contract.Vectors) != 22 {
		t.Fatal("incomplete shared contract")
	}
	for _, c := range contract.Vectors {
		t.Run(c.Name, func(t *testing.T) {
			data, err := hex.DecodeString(c.DataHex)
			if err != nil {
				t.Fatal(err)
			}
			got := vowifi.DecodeIKEBackoffTimer(data)
			if string(got.Status) != c.Status {
				t.Fatalf("%+v, want %s", got, c.Status)
			}
			if c.Seconds != nil && got.Duration != time.Duration(*c.Seconds)*time.Second {
				t.Fatalf("duration %s, seconds %d", got.Duration, *c.Seconds)
			}
			if c.Seconds == nil && got.Duration != 0 {
				t.Fatal("invalid/deactivated timer invented a duration")
			}
		})
	}
}

func TestBackoffEvidenceScopeAndAmbiguity(t *testing.T) {
	for kind, scope := range map[uint16]vowifi.EPDGRejectionScope{10500: vowifi.EPDGRejectionEndpoint, 9002: vowifi.EPDGRejectionPLMNAPN, 9000: vowifi.EPDGRejectionPLMN, 9001: vowifi.EPDGRejectionPLMN, 9003: vowifi.EPDGRejectionPLMN, 9006: vowifi.EPDGRejectionPLMN, 11001: vowifi.EPDGRejectionPLMN, 11011: vowifi.EPDGRejectionPLMN} {
		items := []payload{makeNotify(kind, nil), makeNotify(41041, []byte{1, 0xAA})}
		if authenticatedNetworkRejection(items, false) != nil {
			t.Fatal("untrusted notification became policy")
		}
		got := authenticatedNetworkRejection(items, true)
		if got == nil || got.NotifyType != kind || got.Scope != scope || got.Backoff.Duration != 10*time.Minute {
			t.Fatalf("scope for %d: %+v", kind, got)
		}
	}
	for _, mode := range []string{"duplicate-error", "mixed-error", "unknown-error", "cause-spi", "cause-data", "truncated", "timer-only"} {
		t.Run(mode, func(t *testing.T) {
			items := []payload{makeNotify(10500, nil), makeNotify(41041, []byte{1, 0xAA})}
			switch mode {
			case "duplicate-error":
				items = append(items, makeNotify(10500, nil))
			case "mixed-error":
				items = append(items, makeNotify(24, nil))
			case "unknown-error":
				items[0] = makeNotify(9999, nil)
			case "cause-spi":
				items[0] = payload{Type: payloadNotify, Body: []byte{0, 1, 0x29, 4, 0}}
			case "cause-data":
				items[0] = makeNotify(10500, []byte{0})
			case "truncated":
				items = append(items, payload{Type: payloadNotify, Body: []byte{0}})
			case "timer-only":
				items = items[1:]
			}
			if got := authenticatedNetworkRejection(items, true); got != nil {
				t.Fatalf("ambiguous evidence: %+v", got)
			}
		})
	}
	items := []payload{makeNotify(10500, nil), makeNotify(41041, []byte{1, 0xAA})}
	items[0].Body[0], items[1].Body[0] = 255, 255 // empty SPI ignores Protocol ID
	if got := authenticatedNetworkRejection(items, true); got == nil || got.Backoff.Duration != 10*time.Minute {
		t.Fatal("ignored Protocol ID blocked valid evidence")
	}
}

type backoffTranscript struct {
	*initialAuthTransport
	behavior string
}

func (tr *backoffTranscript) RoundTripExchange(ctx context.Context, packets [][]byte) ([][]byte, error) {
	encoded, err := tr.initialAuthTransport.RoundTripExchange(ctx, packets)
	if err != nil {
		return nil, err
	}
	h, items, err := decryptPayloads(encoded[0], tr.suite, tr.keys.SKer, tr.keys.SKar)
	if err != nil {
		return nil, err
	}
	later := tr.behavior == "later-error" || tr.behavior == "earlier-timer" || tr.behavior == "later-untrusted"
	if tr.behavior == "no-auth" || tr.behavior == "later-untrusted" {
		items = append(payloadsOfType(items, payloadIDr), payloadsOfType(items, payloadEAP)...)
	}
	if later && h.MessageID == 1 {
		items[len(items)-1] = payload{Type: payloadEAP, Body: []byte{eapRequest, 21, 0, 5, eapTypeIdentity}}
		if tr.behavior == "earlier-timer" {
			items = append(items, makeNotify(41041, []byte{1, 0xAA}))
		}
	} else {
		if later {
			items = nil
		} else {
			items = items[:len(items)-1]
		}
		kind := uint16(10500)
		if tr.behavior == "apn" {
			kind = 9002
		}
		if tr.behavior == "plmn" {
			kind = 9003
		}
		items = append(items, makeNotify(kind, nil))
		if tr.behavior != "missing" && tr.behavior != "earlier-timer" {
			data := []byte{1, 0xAA}
			if tr.behavior == "zero" {
				data[1] = 0
			}
			if tr.behavior == "deactivated" {
				data[1] = 0xE0
			}
			if tr.behavior == "bare" {
				data = data[1:]
			}
			items = append(items, makeNotify(41041, data))
		}
		if tr.behavior == "duplicate-timer" {
			items = append(items, makeNotify(41041, []byte{1, 0}))
		}
		if tr.behavior == "duplicate-error" {
			items = append(items, makeNotify(9002, nil))
		}
	}
	response, err := encryptPayloads(h, items, tr.suite, tr.keys.SKer, tr.keys.SKar, rand.Reader)
	return [][]byte{response}, err
}

func (tr *backoffTranscript) SendSessionPacket(ctx context.Context, packet []byte, esp bool) error {
	if err := tr.initialAuthTransport.SendSessionPacket(ctx, packet, esp); err != nil {
		return err
	}
	if tr.behavior == "delete-error" {
		return errors.New("synthetic DELETE failure")
	}
	return nil
}

func (tr *backoffTranscript) Close() error {
	tr.initialAuthTransport.Close()
	if tr.behavior == "close-error" {
		return errors.New("synthetic close failure")
	}
	return nil
}

func backoffCertificates(t *testing.T) (*rsa.PrivateKey, []byte, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic backoff root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"epdg.example"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * time.Minute), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, UnknownExtKeyUsage: []asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 17}}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return key, der, roots
}

func TestBackoffEvidenceActualProviderPreservesCurrentTrustScopeAndCleanup(t *testing.T) {
	key, der, roots := backoffCertificates(t)
	for _, mode := range []string{"network", "apn", "plmn", "deactivated", "zero", "bare", "missing", "duplicate-timer", "duplicate-error", "no-auth", "wrong-name", "bad-signature", "later-error", "earlier-timer", "later-untrusted", "delete-error", "close-error"} {
		t.Run(mode, func(t *testing.T) {
			name := "epdg.example"
			if mode == "wrong-name" {
				name = "other.example"
			}
			provider, err := NewProvider(Config{Installer: unusedInstaller{}, RootCAs: roots, ServerName: name, APN: "ims-test"})
			if err != nil {
				t.Fatal(err)
			}
			tr := &backoffTranscript{initialAuthTransport: &initialAuthTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, mode: "rsa", key: key, certificate: der}, behavior: mode}
			if mode == "bad-signature" {
				tr.mode = "bad-signature"
			}
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				return tr, nil
			}
			aka := &testAKAProvider{}
			_, err = provider.Start(context.Background(), familyRequest(aka))
			var proof *vowifi.EPDGNetworkRejection
			want := mode != "no-auth" && mode != "wrong-name" && mode != "bad-signature" && mode != "duplicate-error" && mode != "later-untrusted"
			if err == nil || errors.As(err, &proof) != want || !tr.deleted || tr.closes != 1 || aka.calls != 0 || vowifi.EPDGFallbackAllowed(err) {
				t.Fatalf("mode=%s proof=%+v deletes=%v closes=%d AKA=%d err=%v", mode, proof, tr.deleted, tr.closes, aka.calls, err)
			}
			if !want {
				return
			}
			kind, scope := uint16(10500), vowifi.EPDGRejectionEndpoint
			if mode == "apn" {
				kind, scope = 9002, vowifi.EPDGRejectionPLMNAPN
			}
			if mode == "plmn" {
				kind, scope = 9003, vowifi.EPDGRejectionPLMN
			}
			status := vowifi.IKEBackoffTimed
			switch mode {
			case "deactivated":
				status = vowifi.IKEBackoffDeactivated
			case "bare", "duplicate-timer":
				status = vowifi.IKEBackoffInvalid
			case "missing", "earlier-timer":
				status = vowifi.IKEBackoffMissing
			}
			if proof.NotifyType != kind || proof.Scope != scope || proof.Backoff.Status != status || proof.EPDG != "epdg.example" || proof.Endpoint == "" || proof.APN != "ims-test" {
				t.Fatalf("wrong binding: %+v", proof)
			}
			if status == vowifi.IKEBackoffTimed {
				duration := 10 * time.Minute
				if mode == "zero" {
					duration = 0
				}
				if proof.Backoff.Duration != duration {
					t.Fatalf("duration=%s want=%s", proof.Backoff.Duration, duration)
				}
			}
			if (mode == "delete-error" || mode == "close-error") && !strings.Contains(err.Error(), "cleanup incomplete") {
				t.Fatal("cleanup failure erased")
			}
			if strings.Contains(err.Error(), familyRequest(aka).Identity.IMSI) {
				t.Fatal("private identity leaked to diagnostic")
			}
		})
	}
}

func TestBackoffEvidenceProviderCannotInheritAuthenticationAcrossStarts(t *testing.T) {
	key, der, roots := backoffCertificates(t)
	provider, err := NewProvider(Config{Installer: unusedInstaller{}, RootCAs: roots, ServerName: "epdg.example"})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"network", "no-auth"} {
		tr := &backoffTranscript{initialAuthTransport: &initialAuthTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, mode: "rsa", key: key, certificate: der}, behavior: mode}
		provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
			return tr, nil
		}
		_, err = provider.Start(context.Background(), familyRequest(&testAKAProvider{}))
		var proof *vowifi.EPDGNetworkRejection
		if err == nil || errors.As(err, &proof) != (mode == "network") || !tr.deleted || tr.closes != 1 {
			t.Fatalf("mode=%s proof=%+v err=%v", mode, proof, err)
		}
	}
}
