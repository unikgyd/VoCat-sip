package ike

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"vocat/internal/vowifi"
)

const (
	authMethodRSASignature     = 1
	authMethodSharedKeyMIC     = 2
	authMethodECDSASHA256P256  = 9
	authMethodECDSASHA384P384  = 10
	authMethodECDSASHA512P521  = 11
	authMethodDigitalSignature = 14
)

var (
	oidSHA256WithRSA   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidSHA384WithRSA   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 12}
	oidSHA512WithRSA   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 13}
	oidECDSAWithSHA256 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidECDSAWithSHA384 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 3}
	oidECDSAWithSHA512 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 4}
	oidRSAPSS          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 10}
	oidIPsecIKE        = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 17}
)

func responderSignedOctets(
	initialResponse []byte,
	initiatorNonce []byte,
	suite negotiatedSuite,
	skpr []byte,
	idr payload,
) ([]byte, error) {
	idHash, err := prf(suite, skpr, idr.Body)
	if err != nil {
		return nil, err
	}
	signed := make([]byte, 0, len(initialResponse)+len(initiatorNonce)+len(idHash))
	signed = append(signed, initialResponse...)
	signed = append(signed, initiatorNonce...)
	signed = append(signed, idHash...)
	return signed, nil
}

func initiatorSignedOctets(
	initialRequest []byte,
	responderNonce []byte,
	suite negotiatedSuite,
	skpi []byte,
	idi payload,
) ([]byte, error) {
	idHash, err := prf(suite, skpi, idi.Body)
	if err != nil {
		return nil, err
	}
	signed := make([]byte, 0, len(initialRequest)+len(responderNonce)+len(idHash))
	signed = append(signed, initialRequest...)
	signed = append(signed, responderNonce...)
	signed = append(signed, idHash...)
	return signed, nil
}

func makeEAPInitiatorAUTH(
	msk []byte,
	initialRequest []byte,
	responderNonce []byte,
	suite negotiatedSuite,
	skpi []byte,
	idi payload,
) (payload, error) {
	signed, err := initiatorSignedOctets(initialRequest, responderNonce, suite, skpi, idi)
	if err != nil {
		return payload{}, err
	}
	paddedKey, err := prf(suite, msk, []byte("Key Pad for IKEv2"))
	if err != nil {
		return payload{}, err
	}
	authValue, err := prf(suite, paddedKey, signed)
	if err != nil {
		return payload{}, err
	}
	body := make([]byte, 4+len(authValue))
	body[0] = authMethodSharedKeyMIC
	copy(body[4:], authValue)
	return payload{Type: payloadAuth, Body: body}, nil
}

func verifyEAPResponderAUTH(
	auth payload,
	msk []byte,
	initialResponse []byte,
	initiatorNonce []byte,
	suite negotiatedSuite,
	skpr []byte,
	idr payload,
) error {
	if len(auth.Body) < 4 || auth.Body[0] != authMethodSharedKeyMIC {
		return errors.New("ike: final responder AUTH does not use the EAP shared-key MIC")
	}
	signed, err := responderSignedOctets(initialResponse, initiatorNonce, suite, skpr, idr)
	if err != nil {
		return err
	}
	paddedKey, err := prf(suite, msk, []byte("Key Pad for IKEv2"))
	if err != nil {
		return err
	}
	expected, err := prf(suite, paddedKey, signed)
	if err != nil {
		return err
	}
	if len(auth.Body[4:]) != len(expected) || subtle.ConstantTimeCompare(auth.Body[4:], expected) != 1 {
		return errors.New("ike: final responder AUTH is invalid")
	}
	return nil
}

func validateInitialResponderAUTH(
	payloads []payload,
	initialResponse []byte,
	initiatorNonce []byte,
	suite negotiatedSuite,
	skpr []byte,
	expectedIDr string,
	serverName string,
	roots *x509.CertPool,
	pinned crypto.PublicKey,
	allowMissing bool,
) (vowifi.ResponderAUTHStatus, payload, error) {
	idrPayloads := payloadsOfType(payloads, payloadIDr)
	authPayloads := payloadsOfType(payloads, payloadAuth)
	if len(authPayloads) == 0 {
		if len(idrPayloads) > 1 {
			return vowifi.ResponderAUTHInvalid, payload{}, errors.New("ike: duplicate responder identity payload")
		}
		if allowMissing {
			if len(idrPayloads) == 1 {
				return vowifi.ResponderAUTHMissing, idrPayloads[0], nil
			}
			return vowifi.ResponderAUTHMissing, payload{}, nil
		}
		return vowifi.ResponderAUTHMissing, payload{}, vowifi.ErrResponderAUTHRequired
	}
	if len(authPayloads) != 1 || len(idrPayloads) != 1 {
		return vowifi.ResponderAUTHInvalid, payload{}, errors.New("ike: responder AUTH requires exactly one IDr and AUTH payload")
	}
	idr := idrPayloads[0]
	auth := authPayloads[0]
	if len(idr.Body) < 4 || len(auth.Body) < 5 {
		return vowifi.ResponderAUTHInvalid, idr, errors.New("ike: responder IDr or AUTH payload is truncated")
	}
	if err := validateFQDNIDr(idr, expectedIDr, "initial ePDG"); err != nil {
		return vowifi.ResponderAUTHInvalid, idr, err
	}
	publicKey := pinned
	if publicKey == nil {
		certificates, err := parseResponderCertificates(payloads)
		if err != nil {
			return vowifi.ResponderAUTHInvalid, idr, err
		}
		if len(certificates) == 0 {
			return vowifi.ResponderAUTHInvalid, idr, errors.New("ike: responder AUTH has no certificate or pinned public key")
		}
		if err := verifyResponderCertificate(certificates, roots, serverName); err != nil {
			return vowifi.ResponderAUTHInvalid, idr, err
		}
		publicKey = certificates[0].PublicKey
	}
	signed, err := responderSignedOctets(initialResponse, initiatorNonce, suite, skpr, idr)
	if err != nil {
		return vowifi.ResponderAUTHInvalid, idr, err
	}
	if err := verifyDigitalAUTH(publicKey, auth.Body[0], auth.Body[4:], signed); err != nil {
		return vowifi.ResponderAUTHInvalid, idr, fmt.Errorf("ike: invalid responder AUTH: %w", err)
	}
	return vowifi.ResponderAUTHVerified, idr, nil
}

func signatureHashSupport(items []payload) (bool, error) {
	found := false
	for _, item := range payloadsOfType(items, payloadNotify) {
		kind, data, err := parseNotify(item)
		if err != nil {
			return false, err
		}
		if kind != 16431 {
			continue
		}
		if found || item.Body[1] != 0 || len(data) == 0 || len(data)%2 != 0 {
			return false, errors.New("ike: invalid SIGNATURE_HASH_ALGORITHMS")
		}
		found = true
	}
	return found, nil
}

func validateFQDNIDr(idr payload, expectedIDr string, label string) error {
	if len(idr.Body) < 4 {
		return errors.New("ike: responder identity is truncated")
	}
	identityType := idr.Body[0]
	identity := strings.TrimSpace(string(idr.Body[4:]))
	if identityType != 2 {
		return fmt.Errorf("ike: %s IDr must use ID_FQDN, got type %d", label, identityType)
	}
	if identity == "" {
		return fmt.Errorf("ike: %s IDr is empty", label)
	}
	if expectedIDr != "" && !strings.EqualFold(strings.TrimSuffix(identity, "."), strings.TrimSuffix(expectedIDr, ".")) {
		return fmt.Errorf("ike: %s IDr %q does not match %q", label, identity, expectedIDr)
	}
	return nil
}

func parseResponderCertificates(payloads []payload) ([]*x509.Certificate, error) {
	var certificates []*x509.Certificate
	for index, item := range payloadsOfType(payloads, payloadCert) {
		if len(item.Body) < 1 {
			return nil, errors.New("ike: responder certificate payload is truncated")
		}
		if item.Body[0] != 4 {
			if index == 0 {
				return nil, fmt.Errorf("ike: unsupported signing certificate encoding %d", item.Body[0])
			}
			continue // Irrelevant later encodings are neither trust anchors nor URLs to fetch.
		}
		certificate, err := x509.ParseCertificate(item.Body[1:])
		if err != nil {
			return nil, fmt.Errorf("ike: parse responder certificate: %w", err)
		}
		certificates = append(certificates, certificate)
	}
	return certificates, nil
}

func verifyResponderCertificate(certificates []*x509.Certificate, roots *x509.CertPool, serverName string) error {
	if len(certificates) == 0 {
		return errors.New("ike: no responder certificate")
	}
	if certificates[0].IsCA {
		return errors.New("ike: responder signing certificate is a CA")
	}
	for _, extension := range certificates[0].Extensions {
		if extension.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 15}) && certificates[0].KeyUsage&x509.KeyUsageDigitalSignature == 0 {
			return errors.New("ike: responder certificate does not permit digital signatures")
		}
	}
	if roots == nil {
		var err error
		roots, err = x509.SystemCertPool()
		if err != nil {
			return fmt.Errorf("ike: load system certificate roots: %w", err)
		}
	}
	intermediates := x509.NewCertPool()
	for _, certificate := range certificates[1:] {
		intermediates.AddCert(certificate)
	}
	options := x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny}, // IKE's OID is checked on every certificate below.
		DNSName:       strings.TrimSuffix(serverName, "."),
	}
	chains, err := certificates[0].Verify(options)
	if err != nil {
		return fmt.Errorf("ike: verify responder certificate: %w", err)
	}
	for _, chain := range chains {
		allowed := true
		for _, certificate := range chain {
			purpose := len(certificate.ExtKeyUsage) == 0 && len(certificate.UnknownExtKeyUsage) == 0
			for _, usage := range certificate.ExtKeyUsage {
				purpose = purpose || usage == x509.ExtKeyUsageAny
			}
			for _, usage := range certificate.UnknownExtKeyUsage {
				purpose = purpose || usage.Equal(oidIPsecIKE)
			}
			allowed = allowed && purpose
		}
		if allowed {
			return nil
		}
	}
	return errors.New("ike: certificate path does not permit IPsec IKE authentication")
}

func verifyDigitalAUTH(publicKey crypto.PublicKey, method uint8, signature, signed []byte) error {
	switch method {
	case authMethodRSASignature:
		key, ok := publicKey.(*rsa.PublicKey)
		if !ok {
			return errors.New("RSA AUTH used with a non-RSA public key")
		}
		digest := sha1.Sum(signed)
		return rsa.VerifyPKCS1v15(key, crypto.SHA1, digest[:], signature)
	case authMethodECDSASHA256P256:
		return verifyRawECDSA(publicKey, crypto.SHA256, signature, signed)
	case authMethodECDSASHA384P384:
		return verifyRawECDSA(publicKey, crypto.SHA384, signature, signed)
	case authMethodECDSASHA512P521:
		return verifyRawECDSA(publicKey, crypto.SHA512, signature, signed)
	case authMethodDigitalSignature:
		return verifyGenericSignature(publicKey, signature, signed)
	default:
		return fmt.Errorf("unsupported responder AUTH method %d", method)
	}
}

func verifyRawECDSA(publicKey crypto.PublicKey, algorithm crypto.Hash, signature, signed []byte) error {
	key, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("ECDSA AUTH used with a non-ECDSA public key")
	}
	size := (key.Curve.Params().BitSize + 7) / 8
	expected := map[crypto.Hash]int{crypto.SHA256: 256, crypto.SHA384: 384, crypto.SHA512: 521}[algorithm]
	if key.Curve.Params().BitSize != expected {
		return errors.New("ECDSA AUTH method does not match its mandated curve")
	}
	if len(signature) != size*2 {
		return fmt.Errorf("ECDSA signature length %d does not match curve size %d", len(signature), size)
	}
	digest, err := hashSignedOctets(algorithm, signed)
	if err != nil {
		return err
	}
	r := new(big.Int).SetBytes(signature[:size])
	s := new(big.Int).SetBytes(signature[size:])
	if !ecdsa.Verify(key, digest, r, s) {
		return errors.New("ECDSA signature verification failed")
	}
	return nil
}

func verifyGenericSignature(publicKey crypto.PublicKey, encoded, signed []byte) error {
	if len(encoded) < 3 || encoded[0] == 0 || int(encoded[0])+1 >= len(encoded) {
		return errors.New("invalid generic signature AlgorithmIdentifier length")
	}
	length := int(encoded[0])
	var algorithm pkix.AlgorithmIdentifier
	rest, err := asn1.Unmarshal(encoded[1:1+length], &algorithm)
	if err != nil || len(rest) != 0 {
		return errors.New("generic digital signature has an invalid AlgorithmIdentifier")
	}
	signature := encoded[1+length:]
	if algorithm.Algorithm.Equal(oidRSAPSS) {
		return verifyPSSSignature(publicKey, algorithm.Parameters.FullBytes, signature, signed)
	}
	if len(algorithm.Parameters.FullBytes) != 0 && !bytes.Equal(algorithm.Parameters.FullBytes, []byte{5, 0}) {
		return errors.New("unexpected generic signature parameters")
	}
	var hashAlgorithm crypto.Hash
	var isRSA bool
	switch {
	case algorithm.Algorithm.Equal(oidSHA256WithRSA):
		hashAlgorithm, isRSA = crypto.SHA256, true
	case algorithm.Algorithm.Equal(oidSHA384WithRSA):
		hashAlgorithm, isRSA = crypto.SHA384, true
	case algorithm.Algorithm.Equal(oidSHA512WithRSA):
		hashAlgorithm, isRSA = crypto.SHA512, true
	case algorithm.Algorithm.Equal(oidECDSAWithSHA256):
		hashAlgorithm = crypto.SHA256
	case algorithm.Algorithm.Equal(oidECDSAWithSHA384):
		hashAlgorithm = crypto.SHA384
	case algorithm.Algorithm.Equal(oidECDSAWithSHA512):
		hashAlgorithm = crypto.SHA512
	default:
		return fmt.Errorf("unsupported generic signature algorithm %s", algorithm.Algorithm.String())
	}
	digest, err := hashSignedOctets(hashAlgorithm, signed)
	if err != nil {
		return err
	}
	if isRSA {
		key, ok := publicKey.(*rsa.PublicKey)
		if !ok {
			return errors.New("RSA signature used with a non-RSA public key")
		}
		return rsa.VerifyPKCS1v15(key, hashAlgorithm, digest, signature)
	}
	key, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("ECDSA signature used with a non-ECDSA public key")
	}
	if !ecdsa.VerifyASN1(key, digest, signature) {
		return errors.New("ECDSA generic signature verification failed")
	}
	return nil
}

func verifyPSSSignature(publicKey crypto.PublicKey, encoded, signature, signed []byte) error {
	var parameters struct {
		Hash    pkix.AlgorithmIdentifier `asn1:"explicit,optional,tag:0"`
		Mask    pkix.AlgorithmIdentifier `asn1:"explicit,optional,tag:1"`
		Salt    int                      `asn1:"explicit,optional,tag:2,default:20"`
		Trailer int                      `asn1:"explicit,optional,tag:3,default:1"`
	}
	rest, err := asn1.Unmarshal(encoded, &parameters)
	if err != nil || len(rest) != 0 {
		return errors.New("invalid RSA-PSS parameters")
	}
	hash := func(id pkix.AlgorithmIdentifier) crypto.Hash {
		if len(id.Parameters.FullBytes) != 0 && !bytes.Equal(id.Parameters.FullBytes, []byte{5, 0}) {
			return 0
		}
		switch id.Algorithm.String() {
		case "2.16.840.1.101.3.4.2.1":
			return crypto.SHA256
		case "2.16.840.1.101.3.4.2.2":
			return crypto.SHA384
		case "2.16.840.1.101.3.4.2.3":
			return crypto.SHA512
		default:
			return 0
		}
	}
	algorithm := hash(parameters.Hash)
	var maskHash pkix.AlgorithmIdentifier
	rest, err = asn1.Unmarshal(parameters.Mask.Parameters.FullBytes, &maskHash)
	if algorithm == 0 || !parameters.Mask.Algorithm.Equal(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 8}) || err != nil || len(rest) != 0 || hash(maskHash) != algorithm || parameters.Salt != algorithm.Size() || parameters.Trailer != 1 {
		return errors.New("unsupported RSA-PSS hash, mask, salt or trailer")
	}
	key, ok := publicKey.(*rsa.PublicKey)
	if !ok {
		return errors.New("RSA-PSS used with non-RSA key")
	}
	digest, err := hashSignedOctets(algorithm, signed)
	if err != nil {
		return err
	}
	return rsa.VerifyPSS(key, algorithm, digest, signature, &rsa.PSSOptions{SaltLength: algorithm.Size(), Hash: algorithm})
}

func hashSignedOctets(algorithm crypto.Hash, signed []byte) ([]byte, error) {
	switch algorithm {
	case crypto.SHA1:
		sum := sha1.Sum(signed)
		return sum[:], nil
	case crypto.SHA256:
		sum := sha256.Sum256(signed)
		return sum[:], nil
	case crypto.SHA384:
		sum := sha512.Sum384(signed)
		return sum[:], nil
	case crypto.SHA512:
		sum := sha512.Sum512(signed)
		return sum[:], nil
	default:
		return nil, fmt.Errorf("unsupported signature hash %v", algorithm)
	}
}

func equalPublicKeys(first, second crypto.PublicKey) bool {
	firstDER, firstErr := x509.MarshalPKIXPublicKey(first)
	secondDER, secondErr := x509.MarshalPKIXPublicKey(second)
	return firstErr == nil && secondErr == nil && bytes.Equal(firstDER, secondDER)
}
