package ike

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"hash"
	"math/big"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

type initialAuthTransport struct {
	*firstAuthCaptureTransport
	mode        string
	key         *rsa.PrivateKey
	certificate []byte
	initial     []byte
	deleted     bool
	closes      int
}

func (tr *initialAuthTransport) RoundTrip(ctx context.Context, packet []byte) ([]byte, error) {
	tr.calls++
	result, err := tr.answerIKEInit(packet)
	if err != nil {
		return nil, err
	}
	h, body, err := parseIKEPacket(result)
	if err != nil {
		return nil, err
	}
	items, err := parsePayloadChain(h.NextPayload, body)
	if err != nil {
		return nil, err
	}
	if tr.mode == "generic" || tr.mode == "pss" {
		items = append(items, makeNotify(16431, []byte{0, 2}))
	}
	h.NextPayload, body, err = marshalPayloadChain(items)
	if err != nil {
		return nil, err
	}
	tr.initial = h.marshal(body)
	return tr.initial, nil
}

func (tr *initialAuthTransport) RoundTripExchange(ctx context.Context, packets [][]byte) ([][]byte, error) {
	tr.calls++
	h, _, err := decryptPayloads(packets[0], tr.suite, tr.keys.SKei, tr.keys.SKai)
	if err != nil {
		return nil, err
	}
	id := payload{Type: payloadIDr, Body: []byte{2, 0, 0, 0, 'i', 'm', 's'}}
	var makeHash func() hash.Hash = sha256.New
	if tr.suite.PRFID == prfHMACSHA1 {
		makeHash = sha1.New
	}
	mac := hmac.New(makeHash, tr.keys.SKpr)
	mac.Write(id.Body)
	signed := append(append(append([]byte(nil), tr.initial...), tr.nonceI...), mac.Sum(nil)...)
	method := byte(1)
	algorithm := crypto.SHA1
	if tr.mode == "generic" || tr.mode == "pss" || tr.mode == "no-hash-notify" {
		method = 14
		algorithm = crypto.SHA256
	}
	digest, err := hashSignedOctets(algorithm, signed)
	if err != nil {
		return nil, err
	}
	var signature []byte
	if tr.mode == "pss" {
		signature, err = rsa.SignPSS(rand.Reader, tr.key, algorithm, digest, &rsa.PSSOptions{SaltLength: 32, Hash: algorithm})
	} else {
		signature, err = rsa.SignPKCS1v15(rand.Reader, tr.key, algorithm, digest)
	}
	if err != nil {
		return nil, err
	}
	if method == 14 {
		id := pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}, Parameters: asn1.RawValue{Tag: 5}}
		if tr.mode == "pss" {
			var parameters []byte
			parameters, err = asn1.Marshal(struct {
				Hash pkix.AlgorithmIdentifier `asn1:"explicit,tag:0"`
				Mask pkix.AlgorithmIdentifier `asn1:"explicit,tag:1"`
				Salt int                      `asn1:"explicit,tag:2"`
			}{
				Hash: pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}, Parameters: asn1.RawValue{Tag: 5}},
				Mask: pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 8}, Parameters: asn1.RawValue{FullBytes: []byte{0x30, 0x0d, 6, 9, 0x60, 0x86, 0x48, 1, 0x65, 3, 4, 2, 1, 5, 0}}}, Salt: 32})
			if err != nil {
				return nil, err
			}
			id = pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 10}, Parameters: asn1.RawValue{FullBytes: parameters}}
		}
		der, err := asn1.Marshal(id)
		if err != nil {
			return nil, err
		}
		signature = append(append([]byte{byte(len(der))}, der...), signature...)
	}
	if tr.mode == "bad-signature" {
		signature[len(signature)-1] ^= 1
	}
	items := []payload{id, {Type: payloadCert, Body: append([]byte{4}, tr.certificate...)}, {Type: payloadAuth, Body: append([]byte{method, 0, 0, 0}, signature...)}, {Type: payloadEAP, Body: []byte{4, 21, 0, 4}}}
	h.Flags = flagResponse
	encoded, err := encryptPayloads(h, items, tr.suite, tr.keys.SKer, tr.keys.SKar, rand.Reader)
	if err != nil {
		return nil, err
	}
	return [][]byte{encoded}, nil
}
func (tr *initialAuthTransport) SendSessionPacket(ctx context.Context, packet []byte, esp bool) error {
	h, items, err := decryptPayloads(packet, tr.suite, tr.keys.SKei, tr.keys.SKai)
	if err != nil {
		return err
	}
	tr.deleted = h.Exchange == exchangeInformational && len(items) == 1 && items[0].Type == payloadDelete
	return nil
}
func (tr *initialAuthTransport) Close() error { tr.closes++; return nil }

func TestInitialAuthProviderValidatesRealCertificateAndWireSignature(t *testing.T) {
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic IKE root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"rsa", "generic", "pss", "bad-signature", "no-hash-notify", "wrong-host", "untrusted", "tls-purpose"} {
		t.Run(mode, func(t *testing.T) {
			leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"epdg.example"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute * 30), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, UnknownExtKeyUsage: []asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 17}}}
			if mode == "tls-purpose" {
				leaf.UnknownExtKeyUsage = nil
				leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			}
			der, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, rootKey)
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			if mode != "untrusted" {
				roots.AddCert(root)
			}
			name := "epdg.example"
			if mode == "wrong-host" {
				name = "other.example"
			}
			provider, err := NewProvider(Config{Installer: unusedInstaller{}, RootCAs: roots, ServerName: name})
			if err != nil {
				t.Fatal(err)
			}
			tr := &initialAuthTransport{firstAuthCaptureTransport: &firstAuthCaptureTransport{t: t}, mode: mode, key: key, certificate: der}
			provider.transportFactory = func(context.Context, transportConfig, vowifi.ProxyRoute, string) (datagramTransport, error) {
				return tr, nil
			}
			aka := &testAKAProvider{}
			_, err = provider.Start(context.Background(), familyRequest(aka))
			var proof *vowifi.EPDGPreAKAIdentityError
			valid := mode == "rsa" || mode == "generic" || mode == "pss"
			if err == nil || errors.As(err, &proof) != valid || !tr.deleted || tr.closes != 1 || aka.calls != 0 || tr.calls != 2 {
				t.Fatalf("mode=%s proof=%v calls=%d DELETE=%v closes=%d AKA=%d err=%v", mode, proof != nil, tr.calls, tr.deleted, tr.closes, aka.calls, err)
			}
		})
	}
}

func TestInitialAuthLegacyCurvesAndGenericECDSAAreDistinct(t *testing.T) {
	for _, c := range []struct {
		name   string
		curve  elliptic.Curve
		method byte
		valid  bool
	}{
		{"p256", elliptic.P256(), 9, true}, {"p384", elliptic.P384(), 10, true}, {"p521", elliptic.P521(), 11, true},
		{"wrong-curve", elliptic.P384(), 9, false}, {"generic-ecdsa", elliptic.P256(), 14, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			key, err := ecdsa.GenerateKey(c.curve, rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			signed := []byte("independent ECDSA AUTH transcript")
			var digest []byte
			switch c.method {
			case 10:
				d := sha512.Sum384(signed)
				digest = d[:]
			case 11:
				d := sha512.Sum512(signed)
				digest = d[:]
			default:
				d := sha256.Sum256(signed)
				digest = d[:]
			}
			var signature []byte
			if c.method == 14 {
				signature, err = ecdsa.SignASN1(rand.Reader, key, digest)
				if err != nil {
					t.Fatal(err)
				}
				der, _ := asn1.Marshal(pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}})
				signature = append(append([]byte{byte(len(der))}, der...), signature...)
			} else {
				r, s, err := ecdsa.Sign(rand.Reader, key, digest)
				if err != nil {
					t.Fatal(err)
				}
				size := (key.Curve.Params().BitSize + 7) / 8
				signature = make([]byte, 2*size)
				r.FillBytes(signature[:size])
				s.FillBytes(signature[size:])
			}
			if err := verifyDigitalAUTH(&key.PublicKey, c.method, signature, signed); (err == nil) != c.valid {
				t.Fatalf("valid=%v err=%v", c.valid, err)
			}
		})
	}
}

func TestInitialAuthPSSParameterBinding(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signed := []byte("independent PSS AUTH transcript")
	digest := sha256.Sum256(signed)
	sig, err := rsa.SignPSS(rand.Reader, key, crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: 32, Hash: crypto.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	// RFC 7427 A.4.3, independent of the production parameter decoder.
	der, err := hex.DecodeString("304606092a864886f70d01010a3039a00f300d06096086480165030402010500a11c301a06092a864886f70d010108300d06096086480165030402010500a203020120a303020101")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"valid", "wrong-salt", "wrong-mask"} {
		t.Run(name, func(t *testing.T) {
			algorithm := append([]byte(nil), der...)
			if name == "wrong-salt" {
				algorithm[len(algorithm)-6] = 31
			}
			if name == "wrong-mask" {
				algorithm[59] = 2
			}
			encoded := append(append([]byte{byte(len(algorithm))}, algorithm...), sig...)
			if err := verifyDigitalAUTH(&key.PublicKey, 14, encoded, signed); (err == nil) != (name == "valid") {
				t.Fatalf("name=%s err=%v", name, err)
			}
		})
	}
}
