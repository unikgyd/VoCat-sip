//go:build linux

package hostims

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"vocat/internal/vowifi"
)

const (
	nasRegisterIndications = uint16(0x0003)
	nasGetServingSystem    = uint16(0x0024)
)

// NASServingCellSource performs an explicit, owned NAS query, not an MM cache
// read. It shares RuntimePending/RuntimeUncertain with WDS so SIM, PDN and host
// network lifecycle operations cannot pass an unfinished/unknown CID outcome.
// Construction does no I/O. PreparedAccess consumes the bounded observation;
// application mode selection and IMS registration are separate responsibilities.
type NASServingCellSource struct {
	lease *BearerLease
	deps  runtimeOpenerDependencies
}

// NASCellQueryProof proves a bounded solicited query and clean CID ownership,
// NOT continuous cell stability through some later SIP send. QueryWindow is
// the HOST request/response interval, not a fabricated modem sample timestamp.
// The single 0x43 response supplies all Sample fields; separate 0x24 responses
// only gate LTE registration/attachment, and never supply PANI fields.
//
// Before integrating with REGISTER, firmware still needs acceptance for the
// 16-bit EARFCN profile and its query freshness semantics. The public NAS schema
// has no measurement timestamp or complete cell-change delivery guarantee.
// Lack of that metadata does not make a solicited query an MM cached read; it
// limits the guarantees this object can honestly make about later consumption.
type NASCellQueryProof struct {
	source      *NASServingCellSource
	cell        nasLTEServingCell
	runtime     Runtime
	start, end  time.Time
	fingerprint string
}

func (NASServingCellSource) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private owned NAS source>")
}
func (NASCellQueryProof) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private bounded solicited NAS sample; not continuous cell proof>")
}

func NewCellularNASServingCellSource(lease *BearerLease) (*NASServingCellSource, error) {
	return newLinuxNASServingCellSource(lease, runtimeOpenerDependencies{
		qmi: linuxOpenerDependencies(),
		topology: func(lease *BearerLease, pin endpointGuard) (wdsTopology, error) {
			control, ok := pin.(*qmiEndpointPin)
			if !ok {
				return nil, ErrQMIEndpoint
			}
			return pinWDSTopology(lease, control)
		},
	})
}

func newLinuxNASServingCellSource(lease *BearerLease, deps runtimeOpenerDependencies) (*NASServingCellSource, error) {
	if lease == nil || lease.pool == nil || lease.claim == nil || deps.qmi.pin == nil || deps.qmi.acquire == nil || deps.qmi.dial == nil || deps.topology == nil {
		return nil, ErrSelection
	}
	if lease.selection.snapshot.QMIControl == "" {
		return nil, ErrQMIEndpoint
	}
	return &NASServingCellSource{lease: lease, deps: deps}, nil
}

// Observe returns only after NAS and WDS CIDs have exact release ACKs and the
// shared durable query fence is cleared. A canceled caller returns promptly;
// RuntimeSource's independent worker still observes/retains late resources.
func (s *NASServingCellSource) Observe(ctx context.Context) (*NASCellQueryProof, error) {
	if s == nil || s.lease == nil {
		return nil, ErrSelection
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var session *ownedNASCellSession
	reader, err := newRuntimeSource(s.lease, func(worker context.Context, guard func(context.Context) error) (runtimeSession, error) {
		base, err := openCellularRuntime(worker, s.lease, guard, s.deps)
		if err != nil {
			return base, err
		}
		owned, ok := base.(*ownedWDSSession)
		if !ok {
			return base, ErrRuntime
		}
		epoch, ok := owned.proxy.(interface{ nasRevision() uint64 })
		if !ok {
			return base, ErrRuntime
		}
		session = &ownedNASCellSession{ownedWDSSession: owned, caller: ctx, epoch: epoch,
			revision: epoch.nasRevision(), expected: len(s.lease.configuration.Addresses)}
		return session, nil
	})
	if err != nil {
		return nil, err
	}
	runtime, err := reader.Read(ctx)
	if err != nil {
		// Do not inspect session here: caller cancellation can return while
		// its independent owner worker is still constructing/cleaning it.
		return nil, err
	}
	if session == nil || runtime == nil || !session.queried || session.cell == (nasLTEServingCell{}) || session.changed ||
		session.start.IsZero() || session.end.Before(session.start) || session.end.Sub(session.start) > 5*time.Second {
		return nil, ErrServingCellUnavailable
	}
	return &NASCellQueryProof{source: s, cell: session.cell, runtime: runtime.Runtime(),
		start: session.start, end: session.end, fingerprint: runtime.fingerprint}, nil
}

// RuntimeProof returns detached PDN evidence from the SAME owned query as the
// cell sample. It performs no I/O and does not refresh the cell's QueryWindow.
// A later RuntimeProof.Verify uses a fresh caller context and ordinary WDS
// source on the same lease/dependencies, never Observe's NAS-aware closure
// (which captures the original caller and session). It proves only the PDN:
// fresh cell evidence still requires another Observe, not RuntimeProof.Verify.
func (p *NASCellQueryProof) RuntimeProof() (*RuntimeProof, error) {
	if p == nil || p.source == nil || !hashString(p.fingerprint) {
		return nil, ErrRuntime
	}
	source, err := newLinuxRuntimeSource(p.source.lease, p.source.deps)
	if err != nil {
		return nil, err
	}
	return &RuntimeProof{source: source, runtime: cloneRuntime(p.runtime), fingerprint: p.fingerprint}, nil
}

// Sample is the bounded query's value, not a promise it remains current now.
// No PAccessNetworkInfo method is exposed on this proof.
func (p *NASCellQueryProof) Sample() vowifi.LTECell {
	if p == nil {
		return vowifi.LTECell{}
	}
	return p.cell.cell
}
func (p *NASCellQueryProof) QueryWindow() (time.Time, time.Time) {
	if p == nil {
		return time.Time{}, time.Time{}
	}
	return p.start, p.end
}
func (p *NASCellQueryProof) EARFCN() uint32 {
	if p == nil {
		return 0
	}
	return p.cell.earfcn
}
func (p *NASCellQueryProof) PhysicalCellID() uint16 {
	if p == nil {
		return 0
	}
	return p.cell.pci
}
func (p *NASCellQueryProof) Band() uint16 {
	if p == nil {
		return 0
	}
	return p.cell.band
}
func (*NASCellQueryProof) ContinuousCellCoverage() bool { return false }

// Revalidate obtains a NEW bounded sample and compares the same lease,
// topology, PDN, registration and cell. It does not assert a global epoch:
// firmware may omit/coalesce cell events, and neither equal samples nor this
// method prove absence of an unreported A->B->A between separate sessions.
func (p *NASCellQueryProof) Revalidate(ctx context.Context) (*NASCellQueryProof, error) {
	if p == nil || p.source == nil || !hashString(p.fingerprint) {
		return nil, ErrServingCellUnavailable
	}
	current, err := p.source.Observe(ctx)
	if err != nil {
		return nil, err
	}
	if current.fingerprint != p.fingerprint || current.cell != p.cell {
		return nil, ErrChanged
	}
	return current, nil
}

type ownedNASCellSession struct {
	*ownedWDSSession
	caller     context.Context
	epoch      interface{ nasRevision() uint64 }
	revision   uint64
	cid        byte
	expected   int
	rows       []Runtime
	queried    bool
	cell       nasLTEServingCell
	start, end time.Time
}

func (s *ownedNASCellSession) checkEpoch(ctx context.Context) error {
	if err := s.ownedWDSSession.verify(ctx); err != nil {
		return err
	}
	if s.epoch.nasRevision() != s.revision || s.watched && s.proxy.wdsRevision() != s.ownedWDSSession.revision {
		s.changed = true
		return ErrChanged
	}
	return nil
}

func (s *ownedNASCellSession) Query(ctx context.Context, family byte) (Runtime, error) {
	if err := s.caller.Err(); err != nil {
		return Runtime{}, err
	}
	if err := s.checkEpoch(ctx); err != nil {
		return Runtime{}, err
	}
	row, err := s.ownedWDSSession.Query(ctx, family)
	if err != nil {
		return Runtime{}, err
	}
	s.rows = append(s.rows, canonicalRuntime(row))
	if len(s.rows) == s.expected {
		if err := s.queryCell(ctx); err != nil {
			return Runtime{}, err
		}
	}
	return row, nil
}

func (s *ownedNASCellSession) request(ctx context.Context, message uint16, input []qmi.TLV) (*qmi.Packet, error) {
	if err := s.caller.Err(); err != nil {
		return nil, err
	}
	if err := s.checkEpoch(ctx); err != nil {
		return nil, err
	}
	packet, err := s.proxy.SendRequest(ctx, qmi.ServiceNAS, s.cid, message, input)
	if err != nil {
		return nil, ErrRuntime
	}
	if err := s.checkEpoch(ctx); err != nil {
		clearProxyPacket(packet)
		return nil, err
	}
	if packet == nil || packet.ServiceType != qmi.ServiceNAS || packet.ClientID != s.cid || packet.TransactionID == 0 ||
		packet.MessageID != message || packet.IsIndication || len(packet.TLVs) > 32 {
		clearProxyPacket(packet)
		return nil, ErrRuntime
	}
	return packet, nil
}

func nasSuccessfulValues(packet *qmi.Packet) (map[byte][]byte, error) {
	values := make(map[byte][]byte, len(packet.TLVs))
	total := 0
	for _, tlv := range packet.TLVs {
		if _, duplicate := values[tlv.Type]; duplicate {
			return nil, ErrRuntime
		}
		total += len(tlv.Value)
		if total > 16384 {
			return nil, ErrRuntime
		}
		values[tlv.Type] = tlv.Value
	}
	if len(values[2]) != 4 || binary.LittleEndian.Uint16(values[2]) != 0 || binary.LittleEndian.Uint16(values[2][2:]) != 0 {
		return nil, ErrRuntime
	}
	return values, nil
}

func (s *ownedNASCellSession) registeredLTE(ctx context.Context) error {
	packet, err := s.request(ctx, nasGetServingSystem, nil)
	if err != nil {
		return err
	}
	defer clearProxyPacket(packet)
	values, err := nasSuccessfulValues(packet)
	if err != nil {
		return err
	}
	// Official NAS TLV1: registered=1, CS attach (0/1/2), PS attached=1,
	// selected 3GPP=2, one radio interface, LTE=8. Reject mixed RAT/NSA.
	v := values[1]
	if len(v) != 6 || v[0] != 1 || v[1] > 2 || v[2] != 1 || v[3] != 2 || v[4] != 1 || v[5] != 8 {
		return ErrRuntime
	}
	return nil
}

func (s *ownedNASCellSession) queryCell(ctx context.Context) error {
	if s.queried || s.expected < 1 || s.expected > 2 || len(s.rows) != s.expected || len(s.cids) != s.expected {
		return ErrRuntime
	}
	if err := s.caller.Err(); err != nil {
		return err
	}
	if err := s.checkEpoch(ctx); err != nil {
		return err
	}
	cid, err := ownedServiceCID(ctx, s.proxy, qmi.ServiceNAS, 0)
	if err != nil {
		if errors.Is(err, ErrUncertain) {
			s.uncertain = true
		}
		return err
	}
	s.cid = cid // Dedicated NAS ownership is rooted before the next guard/I/O.
	// This changes only reporting on our NAS CID, not RF, attach, profiles,
	// system selection, location services, another client's subscription or SIM.
	// Schema: NAS Register Indications3 TLVs13(serving),18(system),20(RF band).
	// There is no advertised exhaustive per-cell event stream/sequence number.
	packet, err := s.request(ctx, nasRegisterIndications, []qmi.TLV{
		{Type: 0x13, Value: []byte{1}}, {Type: 0x18, Value: []byte{1}}, {Type: 0x20, Value: []byte{1}},
	})
	if err != nil {
		return err
	}
	values, err := nasSuccessfulValues(packet)
	if err == nil && len(values) != 1 {
		err = ErrRuntime
	}
	clearProxyPacket(packet)
	if err != nil {
		return err
	}
	if err := s.registeredLTE(ctx); err != nil {
		return err
	}
	s.start = time.Now()
	packet, err = s.request(ctx, nasGetCellLocationInfo, nil)
	s.end = time.Now()
	if err != nil {
		return err
	}
	// The proxy has already matched the solicited transaction. Re-encode only
	// this detached packet to reuse the strict independent envelope/TLV decoder.
	frame, err := encodeProxyRequest(packet.ServiceType, packet.ClientID, packet.MessageID, packet.TransactionID, packet.TLVs)
	if err == nil {
		frame[6] = 2
		s.cell, err = decodeNASLTECellLocationFrame(frame, s.cid, packet.TransactionID)
	}
	clear(frame)
	clearProxyPacket(packet)
	if err != nil || s.end.Sub(s.start) > 5*time.Second {
		return ErrRuntime
	}
	if err := s.registeredLTE(ctx); err != nil {
		return err
	}
	// Re-query the exact owned WDS CID/family/mux, not an MM cache or a new PDN,
	// so the cell response is bracketed by equivalent actual runtime evidence.
	for i, before := range s.rows {
		if err := s.caller.Err(); err != nil {
			return err
		}
		if err := s.checkEpoch(ctx); err != nil {
			return err
		}
		after, err := readBoundRuntime(ctx, s.proxy, s.cids[i], s.topology.Binding(), before.Family, s.ownedWDSSession.verify)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, canonicalRuntime(after)) {
			return ErrChanged
		}
	}
	if err := s.checkEpoch(ctx); err != nil {
		return err
	}
	s.queried = true
	return nil
}

func (s *ownedNASCellSession) Close(ctx context.Context) error {
	if s.closed {
		return nil
	}
	if s.uncertain {
		return ErrUncertain
	}
	if s.cid != 0 {
		// An observed cell event prevents proof, not release of known CIDs.
		// Hardware/SIM/topology guard failure, however, prevents mutation.
		if s.ownedWDSSession.verify(ctx) != nil {
			s.uncertain = true
			return ErrUncertain
		}
		if _, err := ownedServiceCID(ctx, s.proxy, qmi.ServiceNAS, s.cid); err != nil {
			s.uncertain = true
			return ErrUncertain
		}
		s.cid = 0
	}
	proxy := s.proxy
	if err := s.ownedWDSSession.Close(ctx); err != nil {
		return err
	}
	// Close joins the reader, including indications preceding the final ACK.
	if proxy != nil && s.epoch.nasRevision() != s.revision {
		s.changed = true
	}
	return nil
}
