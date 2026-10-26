//go:build linux

package hostims

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"vocat/internal/vowifi/imssec"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func scopedXFRMFixture() imssec.Config {
	return imssec.Config{LocalIP: net.ParseIP("192.0.2.2"), RemoteIP: net.ParseIP("192.0.2.1"),
		UEClientSPI: 0x10001, UEServerSPI: 0x10002, PCSCFClientSPI: 0x10003, PCSCFServerSPI: 0x10004,
		UEClientPort: 42000, UEServerPort: 42001, PCSCFClientPort: 43000, PCSCFServerPort: 43001,
		EncryptionKey: bytes.Repeat([]byte{0x11}, 16), IntegrityKey: bytes.Repeat([]byte{0x22}, 20)}
}

func TestScopedXFRMPlanAndPrivateKeys(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		config := scopedXFRMFixture()
		if ipv6 {
			config.LocalIP, config.RemoteIP = net.ParseIP("2001:db8::2"), net.ParseIP("2001:db8::1")
		}
		plan, err := buildScopedXFRMPlan(config, 0xa001, 0x7101, 0x7102)
		if err != nil || len(plan.Operations) != 12 {
			t.Fatalf("plan: %v", err)
		}
		for i, op := range plan.Operations {
			if validateScopedXFRMKey(op.Key) != nil || op.Key.Mark != 0xa001 {
				t.Fatal("invalid persistent key")
			}
			if i < 4 && op.Key.Kind != "state" || i >= 4 && op.Key.Kind != "policy" {
				t.Fatal("wrong operation order")
			}
			snapshot, err := scopedXFRMSnapshotOf(op.Key.Kind, op.wireAdd)
			if err != nil || !snapshot.matchesKey(op.Key) || snapshot.MatchFingerprint != op.Fingerprint {
				t.Fatalf("snapshot %d: %v", i, err)
			}
			if i < 4 && (cap(op.wireAdd) != 512 || snapshot.IntegrityAlgorithm != imssec.IntegrityAlgorithm(config) || snapshot.EncryptionAlgorithm != imssec.EncryptionAlgorithm(config)) {
				t.Fatal("SA plan capacity or raw transform metadata does not match")
			}
			encoded, err := json.Marshal(op)
			if err != nil || bytes.Contains(encoded, []byte("wireAdd")) || bytes.Contains(encoded, []byte("EncryptionKey")) {
				t.Fatal("sensitive operation serialized")
			}
			if got := fmt.Sprintf("%+v", op); got != "<private scoped XFRM operation>" {
				t.Fatal("operation formatting is not private")
			}
			var copyKey scopedXFRMKey
			keyJSON, _ := json.Marshal(op.Key)
			if json.Unmarshal(keyJSON, &copyKey) != nil || copyKey != op.Key {
				t.Fatal("persistent key does not round-trip")
			}
		}
		for _, index := range []int{8, 9} {
			if plan.Operations[index].Key.SrcPort != 0 {
				t.Fatal("server inbound misses ephemeral peer")
			}
		}
		for _, index := range []int{10, 11} {
			if plan.Operations[index].Key.DstPort != 0 {
				t.Fatal("server outbound misses ephemeral peer")
			}
		}
		secret := plan.Operations[0].wireAdd[:cap(plan.Operations[0].wireAdd)]
		plan.Clear()
		if !scopedXFRMZero(secret) {
			t.Fatal("plan retained key material")
		}
	}
}

func TestScopedXFRMStrictSnapshot(t *testing.T) {
	plan, err := buildScopedXFRMPlan(scopedXFRMFixture(), 0xa001, 0x7101, 0x7102)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Clear()
	state, policy := plan.Operations[0], plan.Operations[4]
	mutations := map[string]func([]byte) []byte{
		"unknown attribute": func(b []byte) []byte { return scopedXFRMAttr(b, 0x3fff, []byte{1}) },
		"duplicate mark":    func(b []byte) []byte { return scopedXFRMAttr(b, nl.XFRMA_MARK, scopedXFRMMark(0xa001)) },
		"bad attr length":   func(b []byte) []byte { return append(b, 1, 0, 0, 0) },
		"truncated header":  func(b []byte) []byte { return b[:20] },
		"nested flag":       func(b []byte) []byte { return scopedXFRMAttr(b, nl.XFRMA_MARK|0x8000, scopedXFRMMark(0xa001)) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			wire := mutate(append([]byte(nil), state.wireAdd...))
			defer clear(wire)
			if _, err := scopedXFRMSnapshotOf("state", wire); !errors.Is(err, ErrXFRMSchema) {
				t.Fatal("unsupported schema accepted")
			}
		})
	}
	wire := append([]byte(nil), state.wireAdd...)
	defer clear(wire)
	header := nl.DeserializeXfrmUsersaInfo(wire)
	header.Curlft.Packets = 99
	header.Curlft.Bytes = 1000
	header.Curlft.UseTime = 123
	header.Stats.Replay = 2
	header.Seq = 41
	snapshot, err := scopedXFRMSnapshotOf("state", wire)
	if err != nil || snapshot.Fingerprint != state.Fingerprint {
		t.Fatal("dynamic counters changed fingerprint")
	}
	header.Flags ^= nl.XFRM_STATE_NOECN
	changed, err := scopedXFRMSnapshotOf("state", wire)
	if err != nil || changed.Fingerprint == snapshot.Fingerprint {
		t.Fatal("static flags omitted")
	}
	wire = append(wire[:0], policy.wireAdd...)
	nl.DeserializeXfrmUserpolicyInfo(wire).Index = 0x1001
	assigned, err := scopedXFRMSnapshotOf("policy", wire)
	if err != nil || assigned.PolicyIndex != 0x1001 || assigned.MatchFingerprint != policy.Fingerprint || assigned.Fingerprint == policy.Fingerprint {
		t.Fatal("assigned policy identity was silently ignored")
	}
	// Nondefault selector masks and UID are static owner fields, not parsed-JSON omissions.
	nl.DeserializeXfrmUserpolicyInfo(wire).Sel.SportMask = nl.Swap16(0xff00)
	masked, err := scopedXFRMSnapshotOf("policy", wire)
	if err != nil || masked.Fingerprint == assigned.Fingerprint {
		t.Fatal("port mask omitted")
	}
	nl.DeserializeXfrmUserpolicyInfo(wire).Sel.User = 1001
	uid, err := scopedXFRMSnapshotOf("policy", wire)
	if err != nil || uid.Fingerprint == masked.Fingerprint {
		t.Fatal("selector UID omitted")
	}
}

func scopedXFRMNativeNamespace(t *testing.T) (*os.File, uint64) {
	t.Helper()
	if os.Getenv("VOCAT_SCOPED_XFRM_TEST") != "1" {
		t.Skip("requires a fresh NET+MNT namespace and VOCAT_SCOPED_XFRM_TEST=1")
	}
	for _, ns := range []string{"net", "mnt"} {
		self, err := os.Stat("/proc/self/ns/" + ns)
		init, other := os.Stat("/proc/1/ns/" + ns)
		if err != nil || other != nil || os.SameFile(self, init) {
			t.Fatal("refusing non-private namespace")
		}
	}
	links, err := net.Interfaces()
	if err != nil || len(links) != 1 || links[0].Name != "lo" {
		t.Fatal("requires a fresh namespace containing only lo")
	}
	ns, err := os.Open("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ns.Close() })
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	cookie, err := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	if err != nil || cookie == 0 {
		t.Fatal("namespace cookie unavailable")
	}
	return ns, cookie
}

func TestScopedXFRMNative(t *testing.T) {
	ns, cookie := scopedXFRMNativeNamespace(t)
	if bad, err := openScopedXFRM(ns, cookie^0x8000000000000000); err == nil {
		bad.Close()
		t.Fatal("wrong cookie accepted")
	}
	d, err := openScopedXFRM(ns, cookie)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := d.ScopeVacant(ctx, 0xa001, 0x7101, 0x7102); err != nil {
		t.Fatalf("initial vacancy: %v", err)
	}
	config := scopedXFRMFixture()
	var plans []*scopedXFRMPlan
	var fingerprints [][]string
	for _, scope := range []struct{ mark, client, server uint32 }{{0xa001, 0x7101, 0x7102}, {0xb001, 0x7201, 0x7202}} {
		variant := config
		if scope.mark == 0xb001 {
			variant.EncryptionAlgorithm, variant.EncryptionKey = "null", nil
			variant.IntegrityAlgorithm, variant.IntegrityKey = "hmac-md5-96", bytes.Repeat([]byte{0x44}, 16)
		}
		plan, err := buildScopedXFRMPlan(variant, scope.mark, scope.client, scope.server)
		if err != nil {
			t.Fatal(err)
		}
		defer plan.Clear()
		plans = append(plans, plan)
		var observed []string
		for i, op := range plan.Operations {
			if err := d.Add(ctx, op); err != nil {
				t.Fatalf("ADD op %d: %v", i, err)
			}
			snapshot, exists, err := d.Observe(ctx, op.Key)
			if err != nil {
				scopedXFRMLogSchema(t, d, ctx, op.Key)
				t.Fatalf("GET op %d: %v", i, err)
			}
			if !exists || snapshot.MatchFingerprint != op.Fingerprint {
				t.Fatalf("GET op %d changed static shape", i)
			}
			if i < 4 && (snapshot.IntegrityAlgorithm != imssec.IntegrityAlgorithm(variant) || snapshot.EncryptionAlgorithm != imssec.EncryptionAlgorithm(variant)) {
				t.Fatal("native raw transform metadata incorrect")
			}
			observed = append(observed, snapshot.Fingerprint)
		}
		fingerprints = append(fingerprints, observed)
		if err := d.Add(ctx, plan.Operations[0]); !errors.Is(err, ErrXFRMExists) {
			t.Fatalf("EEXIST not explicit: %v", err)
		}
		if err := d.ScopeVacant(ctx, scope.mark, scope.client, scope.server); !errors.Is(err, ErrXFRMConflict) {
			t.Fatalf("occupied scope accepted: %v", err)
		}
	}
	if err := d.ScopeVacant(ctx, 0xc001, 0x7301, 0x7302); err != nil {
		t.Fatalf("unrelated mark incorrectly occupied: %v", err)
	}
	reads := make(chan error, 64)
	for i := 0; i < 64; i++ {
		go func(index int) {
			op := plans[index%2].Operations[index%12]
			snapshot, exists, err := d.Observe(ctx, op.Key)
			if err == nil && (!exists || snapshot.MatchFingerprint != op.Fingerprint) {
				err = ErrXFRMConflict
			}
			reads <- err
		}(i)
	}
	for i := 0; i < 64; i++ {
		if err := <-reads; err != nil {
			t.Fatalf("serialized concurrent GET: %v", err)
		}
	}
	if err := d.ScopeVacant(ctx, 0xc001, 0x7101, 0x7302); !errors.Is(err, ErrXFRMConflict) {
		t.Fatal("reqid collision accepted")
	}
	if err := d.Delete(ctx, plans[0].Operations[0].Key, strings.Repeat("0", 64)); !errors.Is(err, ErrXFRMConflict) {
		t.Fatal("mismatching recorded fingerprint deleted state")
	}
	// A call made from another thread namespace still uses the held socket;
	// constructing a second driver there explicitly opens in the held namespace.
	threadResult := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		original, e := os.Open("/proc/thread-self/ns/net")
		if e != nil {
			runtime.UnlockOSThread()
			threadResult <- e
			return
		}
		defer original.Close()
		if e = unix.Unshare(unix.CLONE_NEWNET); e != nil {
			runtime.UnlockOSThread()
			threadResult <- e
			return
		}
		_, exists, e := d.Observe(ctx, plans[0].Operations[0].Key)
		if e == nil && !exists {
			e = ErrXFRMConflict
		}
		if e == nil {
			var other *scopedXFRMDriver
			other, e = openScopedXFRM(ns, cookie)
			if e == nil {
				_, exists, e = other.Observe(ctx, plans[0].Operations[0].Key)
				if !exists && e == nil {
					e = ErrXFRMConflict
				}
				other.Close()
			}
		}
		if unix.Setns(int(original.Fd()), unix.CLONE_NEWNET) != nil {
			threadResult <- ErrXFRMRejected
			return
		}
		runtime.UnlockOSThread()
		threadResult <- e
	}()
	if err := <-threadResult; err != nil {
		t.Fatalf("namespace-pinned GET/open: %v", err)
	}
	for p := len(plans) - 1; p >= 0; p-- {
		for i := 11; i >= 0; i-- {
			if err := d.Delete(ctx, plans[p].Operations[i].Key, fingerprints[p][i]); err != nil {
				t.Fatalf("DELETE %d/%d: %v", p, i, err)
			}
		}
	}
	if err := d.ScopeVacant(ctx, 0xa001, 0x7101, 0x7102); err != nil {
		t.Fatalf("cleanup left scope occupied: %v", err)
	}
	// Policies alone reserve reqids, even before a state pair is installed.
	op := plans[0].Operations[4]
	if err := d.Add(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := d.ScopeVacant(ctx, 0xc001, 0x7101, 0x7302); !errors.Is(err, ErrXFRMConflict) {
		t.Fatalf("policy-only reqid collision ignored: %v", err)
	}
	observed, exists, err := d.Observe(ctx, op.Key)
	if err != nil || !exists {
		t.Fatal("policy observation failed")
	}
	if err := d.Delete(ctx, op.Key, observed.Fingerprint); err != nil {
		t.Fatal(err)
	}
	scopedXFRMNativeForeignMarks(t, d, ctx)
	ipv6 := scopedXFRMFixture()
	ipv6.LocalIP, ipv6.RemoteIP = net.ParseIP("2001:db8::2"), net.ParseIP("2001:db8::1")
	ipv6.EncryptionAlgorithm, ipv6.EncryptionKey = "des-ede3-cbc", bytes.Repeat([]byte{0x33}, 24)
	v6plan, err := buildScopedXFRMPlan(ipv6, 0x6e000001, 0x91000001, 0x91000002)
	if err != nil {
		t.Fatal(err)
	}
	defer v6plan.Clear()
	var v6fingerprints []string
	for i, op := range v6plan.Operations {
		if err := d.Add(ctx, op); err != nil {
			t.Fatalf("IPv6/3DES ADD %d: %v", i, err)
		}
		observed, exists, err := d.Observe(ctx, op.Key)
		if err != nil || !exists || observed.MatchFingerprint != op.Fingerprint {
			t.Fatalf("IPv6/3DES GET %d: %v", i, err)
		}
		if i < 4 && (observed.IntegrityAlgorithm != "hmac-sha-1-96" || observed.EncryptionAlgorithm != "des-ede3-cbc") {
			t.Fatal("IPv6/3DES transform metadata incorrect")
		}
		v6fingerprints = append(v6fingerprints, observed.Fingerprint)
	}
	for i := 11; i >= 0; i-- {
		if err := d.Delete(ctx, v6plan.Operations[i].Key, v6fingerprints[i]); err != nil {
			t.Fatalf("IPv6/3DES DELETE %d: %v", i, err)
		}
	}
	// Socket policies use wildcard selectors and are not absent just because
	// there are no global policies. They must also reserve their reqid.
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "synthetic-socket-policy")
	raw, err := file.SyscallConn()
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	p := socketPolicyFixture()
	p.ReqID = 0x7501
	if err := RequireSocketESP(raw, p); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := d.ScopeVacant(ctx, 0xc001, 0x7501, 0x7502); !errors.Is(err, ErrXFRMConflict) {
		file.Close()
		t.Fatalf("socket policy reqid collision ignored: %v", err)
	}
	file.Close()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Observe(ctx, plans[0].Operations[0].Key); !errors.Is(err, ErrXFRMClosed) {
		t.Fatalf("closed driver fallback: %v", err)
	}
	// Repeated/concurrent Close is idempotent and never constructs a new fd.
	var closers sync.WaitGroup
	for i := 0; i < 16; i++ {
		closers.Add(1)
		go func() { defer closers.Done(); _ = d.Close() }()
	}
	closers.Wait()
	// Deliberately send one ADD without requesting an ACK: timeout must fence,
	// even though a newly opened observer subsequently sees the installed SA.
	d2, err := openScopedXFRM(ns, cookie)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	short, stop := context.WithTimeout(ctx, 80*time.Millisecond)
	if err := d2.lock(short); err != nil {
		t.Fatal(err)
	}
	_, err = d2.exchange(short, nl.XFRM_MSG_NEWSA, unix.NLM_F_CREATE|unix.NLM_F_EXCL, plans[0].Operations[0].wireAdd, 0, false)
	d2.unlock()
	stop()
	if !errors.Is(err, ErrXFRMUnknown) {
		t.Fatalf("lost ACK not uncertain: %v", err)
	}
	if err := d2.Add(ctx, plans[0].Operations[1]); !errors.Is(err, ErrXFRMUnknown) {
		t.Fatal("uncertain driver accepted another mutation")
	}
	observer, err := openScopedXFRM(ns, cookie)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	if _, exists, err := observer.Observe(ctx, plans[0].Operations[0].Key); err != nil || !exists {
		t.Fatal("no-ACK ADD was not actually installed")
	}
	t.Log("two IPv4 scopes coexist with identical endpoint/SPI tuples (AES/SHA1 and NULL/MD5); IPv6/3DES-SHA1 also verified; 12 states+24 policies ACK/readback/delete; namespace pinning, EEXIST, state/policy/socket reqid collisions, masked/unmarked foreign states, strict fingerprint, concurrent closed fence and lost-ACK fence passed")
	// The last uncertain SA is intentionally not adopted/deleted. Destruction
	// of this wholly private namespace is the only recovery authority here.
}

func scopedXFRMNativeForeignMarks(t *testing.T, d *scopedXFRMDriver, ctx context.Context) {
	t.Helper()
	config := scopedXFRMFixture()
	config.UEClientSPI += 0x10000
	config.UEServerSPI += 0x10000
	config.PCSCFClientSPI += 0x10000
	config.PCSCFServerSPI += 0x10000
	plan, err := buildScopedXFRMPlan(config, 0xd001, 0x8101, 0x8102)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Clear()
	state := plan.Operations[0]
	stripMark := func(wire []byte, headerSize int) []byte {
		attrs, err := scopedXFRMAttributes(wire[headerSize:])
		if err != nil {
			t.Fatal(err)
		}
		out := append([]byte(nil), wire[:headerSize]...)
		for _, attr := range attrs {
			if attr.kind != nl.XFRMA_MARK {
				out = scopedXFRMAttr(out, attr.kind, attr.value)
			}
		}
		return out
	}
	mutate := func(message uint16, wire []byte) {
		t.Helper()
		if err := d.lock(ctx); err != nil {
			t.Fatal(err)
		}
		_, err := d.exchange(ctx, message, unix.NLM_F_ACK|func() uint16 {
			if message == nl.XFRM_MSG_NEWSA {
				return unix.NLM_F_CREATE | unix.NLM_F_EXCL
			}
			return 0
		}(), wire, 0, false)
		d.unlock()
		if err != nil {
			t.Fatalf("test-owned foreign fixture mutation: %v", err)
		}
	}
	deleteWire := stripMark(scopedXFRMRequestKey(state.Key, 0), nl.SizeofXfrmUsersaId)
	defer clear(deleteWire)
	unmarked := stripMark(state.wireAdd, nl.SizeofXfrmUsersaInfo)
	defer clear(unmarked)
	mutate(nl.XFRM_MSG_NEWSA, unmarked)
	if err := d.ScopeVacant(ctx, 0xa001, 0x7101, 0x7102); err != nil {
		t.Fatalf("unrelated unmarked SA occupied all marks: %v", err)
	}
	if err := d.ScopeVacant(ctx, 0xa001, 0x8101, 0x7102); !errors.Is(err, ErrXFRMConflict) {
		t.Fatal("unmarked reqid collision ignored")
	}
	mutate(nl.XFRM_MSG_DELSA, deleteWire)
	mask := (&nl.XfrmMark{Value: 0, Mask: 0xffff0000}).Serialize()
	masked := scopedXFRMAttr(append([]byte(nil), unmarked...), nl.XFRMA_MARK, mask)
	defer clear(masked)
	mutate(nl.XFRM_MSG_NEWSA, masked)
	if err := d.ScopeVacant(ctx, 0xa001, 0x7101, 0x7102); !errors.Is(err, ErrXFRMConflict) {
		t.Fatalf("zero-valued masked mark overlap ignored: %v", err)
	}
	if err := d.ScopeVacant(ctx, 0x100a001, 0x7101, 0x7102); err != nil {
		t.Fatalf("nonoverlapping mark occupied: %v", err)
	}
	mutate(nl.XFRM_MSG_DELSA, scopedXFRMAttr(deleteWire, nl.XFRMA_MARK, mask))
}

func scopedXFRMLogSchema(t *testing.T, d *scopedXFRMDriver, ctx context.Context, key scopedXFRMKey) {
	t.Helper()
	if d.lock(ctx) != nil {
		return
	}
	defer d.unlock()
	message, response := uint16(nl.XFRM_MSG_GETSA), uint16(nl.XFRM_MSG_NEWSA)
	size := nl.SizeofXfrmUsersaInfo
	if key.Kind == "policy" {
		message, response = nl.XFRM_MSG_GETPOLICY, nl.XFRM_MSG_NEWPOLICY
		size = nl.SizeofXfrmUserpolicyInfo
	}
	replies, err := d.exchange(ctx, message, unix.NLM_F_ACK, scopedXFRMRequestKey(key, 0), response, false)
	defer scopedXFRMClearReplies(replies)
	if err != nil || len(replies) != 1 || len(replies[0]) < size {
		return
	}
	attrs, err := scopedXFRMAttributes(replies[0][size:])
	if err != nil {
		return
	}
	for _, attr := range attrs {
		t.Logf("schema metadata only: kind=%s attr=%d length=%d", key.Kind, attr.kind, len(attr.value))
	}
}
