package hostims

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func loadSIMContract(t *testing.T, name string, result any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, result); err != nil {
		t.Fatal(err)
	}
}
func cardHex(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestCellularSIMSharedAKAContract(t *testing.T) {
	var contract struct {
		Version int
		Cases   []struct{ ID, Raw, Outcome, RES, CK, IK, AUTS string }
	}
	loadSIMContract(t, "usim-aka-response-v1.json", &contract)
	if contract.Version != 1 || len(contract.Cases) < 20 {
		t.Fatal("missing contract")
	}
	for _, row := range contract.Cases {
		t.Run(row.ID, func(t *testing.T) {
			result, err := parseCardAKA(cardHex(t, row.Raw))
			defer clearCardAKA(&result)
			switch row.Outcome {
			case "success":
				if err != nil || result.SynchronizationFailure || hex.EncodeToString(result.RES) != row.RES || hex.EncodeToString(result.CK) != row.CK || hex.EncodeToString(result.IK) != row.IK {
					t.Fatal("AKA success mismatch")
				}
			case "sync":
				if err != nil || !result.SynchronizationFailure || hex.EncodeToString(result.AUTS) != row.AUTS || len(result.RES)+len(result.CK)+len(result.IK) != 0 {
					t.Fatal("AKA sync mismatch")
				}
			default:
				if err == nil || len(result.RES)+len(result.CK)+len(result.IK)+len(result.AUTS) != 0 {
					t.Fatal("invalid AKA exported key material")
				}
			}
		})
	}
}
func TestCellularSIMSharedICCIDContract(t *testing.T) {
	var contract struct {
		Cases []struct {
			ID, Raw string
			ICCID   *string
		}
	}
	loadSIMContract(t, "sim-identity-bcd-v1.json", &contract)
	for _, row := range contract.Cases {
		t.Run(row.ID, func(t *testing.T) {
			value, err := decodeCardICCID(cardHex(t, row.Raw))
			if row.ICCID == nil {
				if err == nil || value != "" {
					t.Fatal("bad ICCID accepted")
				}
				return
			}
			if err != nil || value != *row.ICCID {
				t.Fatal("ICCID mismatch")
			}
		})
	}
}
func TestCellularSIMSharedISIMTextContract(t *testing.T) {
	var contract struct {
		Cases []struct {
			ID, Raw string
			Text    *string
		}
	}
	loadSIMContract(t, "isim-identity-v1.json", &contract)
	for _, row := range contract.Cases {
		t.Run(row.ID, func(t *testing.T) {
			value, err := decodeISIMText(cardHex(t, row.Raw))
			if row.Text == nil {
				if err == nil || value != "" {
					t.Fatal("bad identity TLV accepted")
				}
				return
			}
			if err != nil || value != *row.Text {
				t.Fatal("identity TLV mismatch")
			}
		})
	}
}
func encodeCardIMSI(value string) []byte {
	marker := byte(1)
	if len(value)%2 == 1 {
		marker = 9
	}
	raw := []byte{0, (value[0]-'0')<<4 | marker}
	for i := 1; i < len(value); i += 2 {
		b := byte(0xf0) | (value[i] - '0')
		if i+1 < len(value) {
			b = (value[i+1]-'0')<<4 | (value[i] - '0')
		}
		raw = append(raw, b)
	}
	raw[0] = byte(len(raw) - 1)
	return raw
}
func TestCellularSIMIMSIAndChannelValidation(t *testing.T) {
	for _, imsi := range []string{"310260123456780", "23415123456789"} {
		raw := encodeCardIMSI(imsi)
		if got, err := decodeCardIMSI(append(raw, 0xff)); err != nil || got != imsi {
			t.Fatal("valid IMSI rejected")
		}
		for _, bad := range [][]byte{raw[:len(raw)-1], append(append([]byte(nil), raw...), 0), {8, 0xff}, {0}} {
			if got, err := decodeCardIMSI(bad); err == nil || got != "" {
				t.Fatal("invalid IMSI accepted")
			}
		}
		raw[1] ^= 8
		if _, err := decodeCardIMSI(raw); err == nil {
			t.Fatal("wrong IMSI parity accepted")
		}
	}
	for ch := byte(0); ch < 21; ch++ {
		cla, err := channelCLA(ch)
		if ch < 1 || ch > 19 {
			if err == nil {
				t.Fatal("bad channel accepted")
			}
			continue
		}
		want := ch
		if ch > 3 {
			want = 0x40 | (ch - 4)
		}
		if err != nil || cla != want {
			t.Fatal("CLA mismatch")
		}
	}
}
func statusCard(apps ...uimApplication) []byte {
	raw := []byte{1, 0, 3, 3, 0, byte(len(apps))}
	for _, app := range apps {
		raw = append(raw, app.Kind, app.State, 0, 0, 0, 0, byte(len(app.AID)))
		raw = append(raw, app.AID...)
		raw = append(raw, 0, 0, 3, 3, 0, 3, 3)
	}
	return raw
}
func statusEnvelope(primary uint16, cards ...[]byte) []byte {
	raw := []byte{0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, byte(len(cards))}
	binary.LittleEndian.PutUint16(raw, primary)
	for _, card := range cards {
		raw = append(raw, card...)
	}
	return raw
}
func TestCellularSIMPrimarySubscriptionNotFirstCard(t *testing.T) {
	usim := uimApplication{Kind: 2, State: 7, AID: append(append([]byte(nil), usimPrefix...), 0xaa)}
	isim := uimApplication{Kind: 5, State: 7, AID: append(append([]byte(nil), isimPrefix...), 0xbb)}
	raw := statusEnvelope(0x0101, statusCard(usim), statusCard(isim, usim))
	card, err := parseUIMCardStatus(raw)
	if err != nil || card.Slot != 2 || !card.HasISIM || !bytes.Equal(card.USIM, usim.AID) || !bytes.Equal(card.ISIM, isim.AID) {
		t.Fatal("wrong subscription selected")
	}
	for n := 0; n < len(raw); n++ {
		if _, err := parseUIMCardStatus(raw[:n]); err == nil {
			t.Fatal("truncated card status accepted")
		}
	}
	for _, bad := range [][]byte{statusEnvelope(0xffff, statusCard(usim)), statusEnvelope(1, statusCard(usim)), statusEnvelope(0x100, statusCard(usim)), append(append([]byte(nil), raw...), 0)} {
		if _, err := parseUIMCardStatus(bad); err == nil {
			t.Fatal("invalid primary status accepted")
		}
	}
	isim.State = 2
	if _, err := parseUIMCardStatus(statusEnvelope(0, statusCard(usim, isim))); err == nil {
		t.Fatal("broken ISIM treated as absent")
	}
	isim.State = 7
	if _, err := parseUIMCardStatus(statusEnvelope(0, statusCard(usim, isim, isim))); err == nil {
		t.Fatal("ambiguous ISIM accepted")
	}
}
func TestCellularSIMAKATruncatedResponsesExportNoKeys(t *testing.T) {
	raw := cardHex(t, "db081111111111111111102222222222222222222222222222222210333333333333333333333333333333339000")
	for n := 0; n < len(raw)-2; n++ {
		result, err := parseCardAKA(append(append([]byte(nil), raw[:n]...), 0x90, 0))
		if err == nil || len(result.RES)+len(result.CK)+len(result.IK)+len(result.AUTS) != 0 {
			t.Fatal("truncated response exported keys")
		}
	}
}

func FuzzCellularSIMCodecs(f *testing.F) {
	for _, raw := range [][]byte{nil, {0x80, 3, 'a', 'b', 'c'}, {0xdb, 4, 1, 2, 3, 4, 0x90, 0}, encodeCardIMSI("310260123456780"), statusEnvelope(0, statusCard(uimApplication{Kind: 2, State: 7, AID: usimPrefix}))} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = parseUIMCardStatus(raw)
		_, _ = decodeCardICCID(raw)
		_, _ = decodeCardIMSI(raw)
		_, _ = decodeISIMText(raw)
		result, err := parseCardAKA(raw)
		if err != nil && (len(result.RES)+len(result.CK)+len(result.IK)+len(result.AUTS) != 0) {
			t.Fatal("malformed input exported key material")
		}
		clearCardAKA(&result)
		_, _, _ = selectedUIMSlot(raw, 1, 1)
	})
}
