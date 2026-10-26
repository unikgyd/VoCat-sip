package server

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"

	"vocat/internal/device"
	"vocat/internal/exportproxy"
	"vocat/internal/store"
)

// User-plane work belongs to a confirmed data session, just like its monitor.
// It cannot keep an interface alive across stop/recovery, a task or a SIM swap.
// Keep superseded transfers until release so cleanup can join every socket.
type cellularDataTransfer struct {
	id      string
	session *cellularDataSession
	cancel  context.CancelFunc
	done    chan struct{}
}

type cellularDataTransferBinding struct {
	Interface     string
	Index         int
	USBGeneration string
}

func (runtime *cellularDataRuntime) acquireTransfer(ctx context.Context, id, physical, iccid string, binding cellularDataTransferBinding) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	runtime.mu.Lock()
	entry := runtime.entries[id]
	if runtime.quiescing[id] || entry == nil || entry.running || !entry.status.DesiredEnabled ||
		!entry.status.Connected || entry.status.Phase != "connected" || entry.status.MaintenancePhase != "" ||
		entry.session == nil || !entry.session.owned || entry.session.phase != cellularDataSessionActive || entry.session.physicalID != physical ||
		iccid == "" || entry.session.identity != iccid {
		runtime.mu.Unlock()
		return nil, nil, device.ErrDataCleanupPending
	}
	if entry.session.binding == nil {
		captured := binding
		entry.session.binding = &captured
	} else if *entry.session.binding != binding {
		runtime.mu.Unlock()
		return nil, nil, device.ErrSIMObservation
	}
	operation, cancel := context.WithCancel(ctx)
	transfer := &cellularDataTransfer{id: id, session: entry.session, cancel: cancel, done: make(chan struct{})}
	if runtime.transfers == nil {
		runtime.transfers = make(map[*cellularDataTransfer]struct{})
	}
	runtime.transfers[transfer] = struct{}{}
	runtime.mu.Unlock()
	var once sync.Once
	return operation, func() {
		once.Do(func() {
			cancel()
			runtime.mu.Lock()
			delete(runtime.transfers, transfer)
			close(transfer.done)
			runtime.mu.Unlock()
		})
	}, nil
}

func (s *Server) acquireExportProxyAccess(ctx context.Context, config exportproxy.Config) (context.Context, func(), error) {
	if !s.developerActive(ctx) {
		return nil, nil, exportproxy.ErrDisabled
	}
	return s.acquireCellularTransfer(ctx, config.DeviceID, config.Interface)
}

func (s *Server) acquireCellularTransfer(ctx context.Context, id, networkInterface string) (context.Context, func(), error) {
	unlock, err := s.acquireLineRead(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	config, err := s.store.Device(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if !store.CellularDataEnabled(config) || config.Interface == "" || config.Interface != networkInterface {
		return nil, nil, device.ErrDataBackendUnavailable
	}
	entry, physical, present := s.physicalForConfig(config)
	if !present || entry.Snapshot == nil || entry.Candidate.NetworkInterface != networkInterface {
		return nil, nil, device.ErrSIMObservation
	}
	iccid := strings.TrimSpace(entry.Snapshot.ICCID)
	if !validICCID(iccid) || config.Telephony != nil && config.Telephony.ICCID != iccid {
		return nil, nil, device.ErrSIMObservation
	}
	iface, err := net.InterfaceByName(networkInterface)
	if err != nil || iface.Index <= 0 {
		return nil, nil, device.ErrDataBackendUnavailable
	}
	binding := cellularDataTransferBinding{Interface: networkInterface, Index: iface.Index, USBGeneration: entry.Candidate.USBGeneration}
	return s.cellularDataRuntime().acquireTransfer(ctx, id, physical, iccid, binding)
}

// Caller holds runtime.mu. Cancellation only signals socket owners; joining
// always happens outside that mutex and before the next bearer operation.
func (runtime *cellularDataRuntime) cancelTransfersLocked(id string, session *cellularDataSession) []<-chan struct{} {
	var done []<-chan struct{}
	for transfer := range runtime.transfers {
		if id != "" && transfer.id != id || session != nil && transfer.session != session {
			continue
		}
		transfer.cancel()
		done = append(done, transfer.done)
	}
	return done
}

func waitCellularTransfers(ctx context.Context, done []<-chan struct{}) error {
	for _, stopped := range done {
		select {
		case <-stopped:
		case <-ctx.Done():
			return errors.Join(device.ErrDataCleanupPending, ctx.Err())
		}
	}
	return nil
}

func (runtime *cellularDataRuntime) activeDataGeneration(id, iccid string) uint64 {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	entry := runtime.entries[id]
	if runtime.quiescing[id] || entry == nil || entry.running || !entry.status.DesiredEnabled || !entry.status.Connected ||
		entry.status.Phase != "connected" || entry.status.MaintenancePhase != "" || entry.session == nil || !entry.session.owned ||
		entry.session.phase != cellularDataSessionActive || entry.session.identity == "" || entry.session.identity != iccid {
		return 0
	}
	// Include every accepted data transition, including a reconnect that kept
	// the same logical session. Old IP lookups must not populate its new exit.
	return entry.status.Revision
}
