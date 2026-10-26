package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"vocat/internal/vowifi/ims"
)

func TestMediaCheckJSONAndRequiredCodec(t *testing.T) {
	var output bytes.Buffer
	if err := runMediaCheck([]string{"--json"}, &output); err != nil {
		t.Fatal(err)
	}
	var status ims.MediaCapabilities
	if err := json.Unmarshal(output.Bytes(), &status); err != nil || len(status.PCMCodecs) != 2 || status.AMRWB.Available {
		t.Fatalf("invalid local diagnostic %s %v", output.String(), err)
	}
	output.Reset()
	err := runMediaCheck([]string{"--json", "--require-amr"}, &output)
	if (err == nil) != status.AMRNB.Available {
		t.Fatalf("require result=%v status=%+v", err, status)
	}
	if !json.Valid(output.Bytes()) {
		t.Fatal("required failure polluted JSON output")
	}
	output.Reset()
	if err := runMediaCheck(nil, &output); err != nil || !strings.Contains(output.String(), "G.711: PCMA / PCMU") {
		t.Fatal("missing readable local diagnostic")
	}
}
func TestMediaCheckRejectsUnknownFlagsAndArguments(t *testing.T) {
	for _, args := range [][]string{{"--repair"}, {"unexpected"}} {
		var output bytes.Buffer
		if err := runMediaCheck(args, &output); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	var output bytes.Buffer
	if err := runMediaCheck([]string{"--help"}, &output); err != nil || !strings.Contains(output.String(), "require-amr") {
		t.Fatal("help failed")
	}
}
