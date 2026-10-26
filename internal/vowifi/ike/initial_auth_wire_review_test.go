package ike

import (
	"bytes"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509/pkix"
	"encoding/asn1"
	"testing"

	"vocat/internal/vowifi"
)

// Independent RFC 7427 framing, rather than using the production AUTH builder.
func TestInitialAuthWireRFC7427GenericFraming(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	initial := []byte("synthetic responder IKE_SA_INIT transcript")
	ni := bytes.Repeat([]byte{0x42}, 32)
	skpr := bytes.Repeat([]byte{0x31}, 32)
	idr := payload{Type: payloadIDr, Body: []byte{2, 0, 0, 0, 'i', 'm', 's'}}
	mac := hmac.New(sha256.New, skpr)
	mac.Write(idr.Body)
	signed := append(append(append([]byte(nil), initial...), ni...), mac.Sum(nil)...)
	sha256Digest := sha256.Sum256(signed)
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sha256Digest[:])
	if err != nil {
		t.Fatal(err)
	}
	algorithm, err := asn1.Marshal(pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}, Parameters: asn1.RawValue{Tag: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if len(algorithm) > 255 {
		t.Fatal("fixture AlgorithmIdentifier does not fit the RFC length octet")
	}
	withLength := append(append([]byte{byte(len(algorithm))}, algorithm...), signature...)
	bareDER := append(append([]byte(nil), algorithm...), signature...)
	badLength := append([]byte(nil), withLength...)
	badLength[0]--
	zeroLength := append([]byte(nil), withLength...)
	zeroLength[0] = 0
	sha1Digest := sha1.Sum(signed)
	classic, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA1, sha1Digest[:])
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		method  byte
		encoded []byte
		valid   bool
	}{
		{"classic-rsa-control", 1, classic, true},
		{"rfc7427-length-octet", 14, withLength, true},
		{"bare-der-is-not-rfc7427", 14, bareDER, false},
		{"wrong-length", 14, badLength, false},
		{"zero-length", 14, zeroLength, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			auth := payload{Type: payloadAuth, Body: append([]byte{c.method, 0, 0, 0}, c.encoded...)}
			status, _, err := validateInitialResponderAUTH([]payload{idr, auth}, initial, ni, negotiatedSuite{PRFID: prfHMACSHA256}, skpr, "ims", "epdg.example", nil, &key.PublicKey, true)
			if (err == nil && status == vowifi.ResponderAUTHVerified) != c.valid {
				t.Fatalf("valid=%v status=%v err=%v", c.valid, status, err)
			}
		})
	}
}
