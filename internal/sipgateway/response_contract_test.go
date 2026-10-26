package sipgateway

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestSharedResponseIdentityContract(t *testing.T) {
	data, err := os.ReadFile("testdata/response-identity-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version int    `json:"version"`
		CallID  string `json:"expected_call_id"`
		Tag     string `json:"expected_from_tag"`
		Cases   []struct {
			Name      string `json:"name"`
			CallID    string `json:"call_id"`
			Tag       string `json:"from_tag"`
			Status    int    `json:"status"`
			Duplicate bool   `json:"duplicate_from"`
			Accept    bool   `json:"accept"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil || fixture.Version != 1 || len(fixture.Cases) != 9 {
		t.Fatal("invalid response contract")
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			r := &runtime{pendingResponses: map[string]responseExpectation{"z9hG4bK-contract": {source: "127.0.0.1:15062", callID: fixture.CallID, sequence: 1, from: "sip:1001@voice.test", to: "sip:1002@voice.test", tag: fixture.Tag}}}
			from := "<sip:1001@voice.test>"
			if test.Tag != "" {
				from += ";tag=" + test.Tag
			}
			extra := ""
			if test.Duplicate {
				extra = "From: " + from + "\r\n"
			}
			wire := fmt.Sprintf("SIP/2.0 %d Test\r\nVia: SIP/2.0/UDP 127.0.0.1:15060;branch=z9hG4bK-contract\r\nFrom: %s\r\nTo: <sip:1002@voice.test>;tag=receiver\r\nCall-ID: %s\r\nCSeq: 1 MESSAGE\r\n%sContent-Length: 0\r\n\r\n", test.Status, from, test.CallID, extra)
			accepted, err := r.responseFilter()(sip.TransportReadProps{RemoteAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 15062}}, []byte(wire))
			if err != nil || (len(accepted) > 0) != test.Accept {
				t.Fatalf("response contract mismatch: %s", test.Name)
			}
		})
	}
}
