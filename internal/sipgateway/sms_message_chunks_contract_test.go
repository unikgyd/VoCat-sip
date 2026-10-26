package sipgateway

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type smsChunksContractRun struct {
	Text   string `json:"text"`
	Repeat int    `json:"repeat"`
}

// Expand explicit fixture data, not the implementation's splitting algorithm.
func smsChunksExpandRuns(t *testing.T, runs []smsChunksContractRun) string {
	t.Helper()
	var text strings.Builder
	for _, run := range runs {
		if run.Repeat < 1 || run.Repeat > 16384 || !utf8.ValidString(run.Text) {
			t.Fatal("invalid explicit shared chunk fixture run")
		}
		text.WriteString(strings.Repeat(run.Text, run.Repeat))
	}
	return text.String()
}

func TestSMSMessageChunksSharedExplicitContractVectors(t *testing.T) {
	data, err := os.ReadFile("../../../contracts/sip-message-parts-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Version             int  `json:"version"`
		RawChunkUTF8Bytes   int  `json:"rawChunkUtf8Bytes"`
		MaxSIPDatagramBytes int  `json:"maxSipDatagramBytes"`
		SinglePartUnchanged bool `json:"singlePartUnchanged"`
		InputAdmission      struct {
			MaxUTF16Units                         int `json:"maxUtf16Units"`
			StrictUnicode, RejectEmpty, RejectNul bool
		} `json:"inputAdmission"`
		Cases []struct {
			ID             string                   `json:"id"`
			Input          []smsChunksContractRun   `json:"input"`
			ExpectedChunks [][]smsChunksContractRun `json:"expectedChunks"`
			WirePrefixes   []string                 `json:"wirePrefixes"`
		} `json:"cases"`
	}
	if json.Unmarshal(data, &contract) != nil || contract.Version != 1 || len(contract.Cases) != 12 ||
		contract.RawChunkUTF8Bytes != 280 || contract.MaxSIPDatagramBytes != 1300 || !contract.SinglePartUnchanged ||
		contract.InputAdmission.MaxUTF16Units != 16384 || !contract.InputAdmission.StrictUnicode || !contract.InputAdmission.RejectEmpty || !contract.InputAdmission.RejectNul {
		t.Fatal("shared application MESSAGE parts contract was not the approved explicit v1 contract")
	}
	seen := map[string]bool{}
	for _, vector := range contract.Cases {
		t.Run(vector.ID, func(t *testing.T) {
			if vector.ID == "" || seen[vector.ID] || len(vector.ExpectedChunks) == 0 || len(vector.ExpectedChunks) != len(vector.WirePrefixes) {
				t.Fatal("invalid/duplicate shared vector")
			}
			seen[vector.ID] = true
			input := smsChunksExpandRuns(t, vector.Input)
			expected := make([]string, len(vector.ExpectedChunks))
			var reconstructed strings.Builder
			for index, runs := range vector.ExpectedChunks {
				content := smsChunksExpandRuns(t, runs)
				if len(content) > 280 {
					t.Fatal("fixed expected raw content exceeded 280 UTF-8 bytes")
				}
				reconstructed.WriteString(content)
				expected[index] = vector.WirePrefixes[index] + content
			}
			if reconstructed.String() != input {
				t.Fatal("fixed expected chunks changed original scalars/text")
			}
			actual, valid := splitMTMessageText(input)
			if !valid || !reflect.DeepEqual(actual, expected) {
				t.Fatalf("splitter differs from explicit vector %q: got %#v want %#v", vector.ID, actual, expected)
			}
		})
	}
}

func TestSMSMessageChunksAdmissionAndActualStoreCapture(t *testing.T) {
	for _, scenario := range []struct {
		name, body string
		valid      bool
	}{
		{"max-ascii-16384", strings.Repeat("A", 16384), true},
		{"max-cjk-16384-49152bytes", strings.Repeat("中", 16384), true},
		{"max-astral-8192-16384units", strings.Repeat("🌍", 8192), true},
		{"over-ascii-16385", strings.Repeat("A", 16385), false},
		{"over-cjk-byte-precap", strings.Repeat("中", 16385), false},
		{"over-astral-8193", strings.Repeat("🌍", 8193), false},
		{"astral-plus-bmp-one-unit-over", strings.Repeat("🌍", 8192) + "a", false},
		{"empty", "", false},
		{"nul", "original\x00text", false},
		{"invalid-utf8", string([]byte{0xff, 0xfe}), false},
		{"encoded-surrogate-is-not-a-scalar", string([]byte{0xed, 0xa0, 0x80}), false},
		{"short-literal-marker", "[1/2] not protocol grouping", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if got := validMTMessageText(scenario.body); got != scenario.valid {
				t.Fatalf("MT UTF-16/scalar admission=%t want=%t", got, scenario.valid)
			}
			message := IncomingSMS{ID: "admission-synthetic", DeviceID: "module-a", Subscription: smsCaptureICCID, Sender: "+15550000001", Text: scenario.body}
			if got := validIncomingSMS(message); got != scenario.valid {
				t.Fatalf("real MT planner admission=%t want=%t", got, scenario.valid)
			}
			if !scenario.valid {
				return
			}
			// Every maximum form exceeds the old 4096-byte receive limit. This
			// is actual atomic capture through Manager-installed Store planning,
			// not a hand-built private ledger or a direct pure planner call.
			ctx, stores, manager, backend, _ := smsMessagePartsFixture(t)
			saved, err := stores[0].SaveSMSMessage(ctx, smsCaptureMessage("capture-"+scenario.name, scenario.body))
			if err != nil {
				t.Fatal(err)
			}
			_, in := smsCaptureSavedInbox(t, stores[1], saved)
			target, exists := in.Targets["1001"]
			if !exists || in.Rejected || target.Code != 0 || target.AttemptToken != "" || in.Message.Text != scenario.body ||
				in.InboxID != saved.ID || in.SourceIndependent || in.SourceKey == "" || in.SourceFingerprint == "" || in.CanonicalReply == nil {
				t.Fatal("actual capture did not atomically preserve a bound full-text Queued plan")
			}
			// No registration means no delivery attempt, even for a large input.
			if err := manager.DeliverSMSOnce(ctx); err != nil {
				t.Fatal(err)
			}
			_, after := smsCaptureSavedInbox(t, stores[1], saved)
			if after.Targets["1001"].Code != 0 || after.Targets["1001"].AttemptToken != "" || !after.Until.Equal(in.Until) || backend.count() != 0 {
				t.Fatal("offline large MT consumed its claim, TTL or carrier transport")
			}
		})
	}
}
