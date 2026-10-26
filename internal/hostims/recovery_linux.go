//go:build linux

package hostims

import (
	"context"
	"reflect"
)

type recoveredNetworkCleanup struct{ network *kernelNetworkLease }

func (r *recoveredNetworkCleanup) Close(ctx context.Context) error {
	n := r.network
	// Both routines retain the same network gate/ledger after cancellation.
	// Never create keys, routes, reservations or a new PDN to recover cleanup.
	if err := runKernelSecurity(ctx, n, func(worker context.Context) error {
		if n.security != nil {
			return n.security.cleanup(worker)
		}
		return nil
	}, nil, nil); err != nil {
		return err
	}
	// If the old process's security record has not yet been reopened, recover
	// its original reservations before attempting any network-object removal.
	if err := recoverRemainingSecurity(ctx, n); err != nil {
		return err
	}
	return n.Close(ctx)
}

func recoverRemainingSecurity(ctx context.Context, n *kernelNetworkLease) error {
	// Snapshot only under the same gate used by late security workers.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-n.gate:
	}
	pending := n.record.Security != nil
	n.gate <- struct{}{}
	if !pending {
		return nil
	}
	return recoverKernelSecurity(ctx, n)
}

func reopenPendingNetwork(ctx context.Context, owner *PendingBearerCleanup) (interface{ Close(context.Context) error }, error) {
	return reopenPendingNetworkWith(ctx, owner, func(lease *BearerLease, table int) (kernelNetworkDriver, error) {
		driver, err := openLinuxNetworkKernel(lease, false)
		if err == nil {
			driver.table = table
		} // Reuse the old scope; never find/create another table.
		return driver, err
	})
}

// The injected opener is private and only supports synthetic/kernel fixtures.
func reopenPendingNetworkWith(ctx context.Context, owner *PendingBearerCleanup, open func(*BearerLease, int) (kernelNetworkDriver, error)) (*recoveredNetworkCleanup, error) {
	if owner == nil || owner.pool == nil || owner.claim == nil || open == nil {
		return nil, ErrSelection
	}
	c := owner.claim
	c.mu.Lock()
	r := c.record
	valid := c.adopted && c.pendingCleanup == owner && c.lock != nil && !c.busy && c.preparedAccess == nil &&
		!c.storageFailed && len(c.retained) == 0 && len(c.networkHolds) == 0 && r.Token == owner.token && r.Stage == "active" &&
		!r.Uncertain && r.Pending == "" && !r.SIMPending && !r.SIMUncertain && !r.RuntimePending && !r.RuntimeUncertain
	record := cloneNetworkClaim(r.Network)
	c.mu.Unlock()
	if !valid || record == nil || !validNetworkClaim(record) || record.Pending != nil || record.Phase == "uncertain" ||
		record.Security != nil && (record.Security.Pending != nil || record.Security.Phase == "uncertain") {
		return nil, ErrUncertain
	}
	for _, op := range record.Operations {
		if !hashString(op.Hash) {
			return nil, ErrUncertain
		}
	}
	if owner.pool.authorize(ctx, owner.selection) != nil {
		return nil, ErrDenied
	}
	bearer, err := owner.pool.owned(ctx, owner.selection, c)
	if err != nil {
		return nil, err
	}
	if !bearer.Connected || bearer.Suspended || !validConfiguration(bearer) {
		return nil, ErrChanged
	}
	configuration := BearerConfiguration{Interface: bearer.Interface, Addresses: append([]BearerAddress(nil), bearer.Addresses...)}
	if record.Configuration != bearerConfigurationKey(configuration) || record.Interface != configuration.Interface {
		return nil, ErrChanged
	}
	lease := &BearerLease{pool: owner.pool, selection: owner.selection, claim: c, token: owner.token, configuration: configuration}
	driver, err := open(lease, record.Table)
	if err != nil {
		return nil, err
	}
	name, index, namespace, anchor := driver.identity()
	if name != record.Interface || index != record.Index || namespace != record.Namespace || anchor != record.Anchor {
		driver.close()
		return nil, ErrChanged
	}
	n := &kernelNetworkLease{bearer: lease, driver: driver, record: record, gate: make(chan struct{}, 1), media: make(map[string]Endpoint)}
	if err := n.parent(ctx); err != nil {
		driver.close()
		return nil, err
	}
	c.mu.Lock()
	if c.pendingCleanup != owner || c.record.Token != owner.token || !reflect.DeepEqual(c.record.Network, record) || len(c.networkHolds) != 0 || c.busy || c.storageFailed {
		c.mu.Unlock()
		driver.close()
		return nil, ErrChanged
	}
	if c.networkHolds == nil {
		c.networkHolds = make(map[string]any)
	}
	c.networkHolds[record.Token] = n
	c.mu.Unlock()
	n.gate <- struct{}{}
	return &recoveredNetworkCleanup{network: n}, nil
}
