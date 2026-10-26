package ims

import (
	"testing"

	"vocat/internal/vowifi"
)

func TestCardProvisionedIdentityUsesDomainNotImpiHostOrGuessedPLMN(t *testing.T) {
	config := Config{HomeDomain: "IMS.Example.NET", PrivateIdentity: "subscriber@private.example.net", PublicIdentity: "sip:+15555550100@public.example.net;user=phone"}
	// EF.AD is unavailable, but all three required ISIM files were read.
	result, err := deriveIdentities(vowifi.SIMIdentity{IMSI: "310260123456780", HomeMCC: "310"}, config)
	if err != nil || result.domain != "ims.example.net" || result.private != config.PrivateIdentity || result.public != config.PublicIdentity || result.user != "+15555550100" || result.temporaryPublic {
		t.Fatal("provisioned identity mismatch", err)
	}
	normalized, err := normalizeConfig(config)
	if err != nil || normalized.HomeDomain != "ims.example.net" {
		t.Fatal("normalization mismatch", err)
	}
	if _, err := deriveIdentities(vowifi.SIMIdentity{IMSI: "310260123456780", HomeMCC: "310"}, Config{}); err == nil {
		t.Fatal("missing MNC guessed outside provisioned identity path")
	}
}
func TestCardProvisionedIdentityRejectsPartialAndInjectedValues(t *testing.T) {
	base := Config{HomeDomain: "ims.example.net", PrivateIdentity: "subscriber@private.example.net", PublicIdentity: "sip:+15555550100@ims.example.net"}
	for _, field := range []string{"private", "public", "domain-injection", "domain-port", "domain-ip", "domain-trailing", "private-injection", "public-injection"} {
		t.Run(field, func(t *testing.T) {
			config := base
			switch field {
			case "private":
				config.PrivateIdentity = ""
			case "public":
				config.PublicIdentity = ""
			case "domain-injection":
				config.HomeDomain = "ims.example.net\r\nX:bad"
			case "domain-port":
				config.HomeDomain = "ims.example.net:5060"
			case "domain-ip":
				config.HomeDomain = "192.0.2.1"
			case "domain-trailing":
				config.HomeDomain = "ims.example.net."
			case "private-injection":
				config.PrivateIdentity = "<sip:subscriber@ims.example.net>"
			case "public-injection":
				config.PublicIdentity = "sip:+15555550100@ims.example.net?header=value"
			}
			if _, err := normalizeConfig(config); err == nil {
				t.Fatal("invalid provisioned identities accepted")
			}
			if _, err := deriveIdentities(vowifi.SIMIdentity{IMSI: "310260123456780", HomeMCC: "310", HomeMNC: "260"}, config); err == nil {
				t.Fatal("invalid identity reached registration builder")
			}
		})
	}
}
