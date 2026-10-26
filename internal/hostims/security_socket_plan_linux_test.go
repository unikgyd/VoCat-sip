//go:build linux

package hostims

import "testing"

func TestSocketCompatibleScopedPlanPreservesDirectionAndOriginalJournalMode(t *testing.T) {
	config := securityOwnerConfig()
	const mark, client, server = uint32(0xa001), uint32(0x8101), uint32(0x8102)
	for _, compat := range []bool{false, true} {
		plan, err := buildScopedXFRMPlanMode(config, mark, client, server, compat)
		if err != nil {
			t.Fatal(err)
		}
		defer plan.Clear()
		agreement := securityAgreement(config)
		record := &securityClaim{Mark: mark, ClientReqID: client, ServerReqID: server, Agreement: &agreement, SocketOutbound: compat}
		for index, op := range plan.Operations {
			wanted := mark
			if compat && (index == 0 || index == 3) {
				wanted = 0
			}
			if op.Key.Mark != wanted {
				t.Fatal("wrong directional SA mark", compat, index)
			}
			expected, err := expectedSecurityXFRMKey(record, uint8(index))
			if err != nil || expected != op.Key {
				t.Fatal("journal lost original directional key", compat, index, err)
			}
			snapshot, err := scopedXFRMSnapshotOf(op.Key.Kind, op.wireAdd)
			if err != nil || !snapshot.matchesKey(op.Key) {
				t.Fatal("kernel key and snapshot disagree", compat, index, err)
			}
			if len(snapshot.reqids) != 1 || snapshot.reqids[0] == 0 {
				t.Fatal("SA/policy lost its exact ReqID")
			}
		}
	}
}
