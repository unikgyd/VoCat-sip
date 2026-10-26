//go:build linux

package hostims

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"reflect"
	"strings"
	"syscall"
	"time"

	"vocat/internal/vowifi"

	"golang.org/x/sys/unix"
)

// kernelNetworkLease is not an IMS bearer yet: production WDS/P-CSCF origin
// proof and application mode ownership must supply its Runtime. Keeping the
// constructor private prevents a UI/manual address from claiming readiness.
type kernelNetworkLease struct {
	bearer            *BearerLease
	driver            kernelNetworkDriver
	record            *networkClaim
	gate              chan struct{}
	signaling         []Endpoint
	closed, uncertain bool
	media             map[string]Endpoint
	proof             *RuntimeProof
	security          *kernelSecurityLease
}

func (*kernelNetworkLease) Format(out fmt.State, _ rune) {
	_, _ = io.WriteString(out, "<private owned kernel network>")
}
func bearerConfigurationKey(configuration BearerConfiguration) string {
	raw, _ := json.Marshal(configuration)
	return hashText(string(raw))
}

func prepareKernelNetwork(ctx context.Context, bearer *BearerLease, runtime Runtime) (*kernelNetworkLease, error) {
	return prepareKernelNetworkWith(ctx, bearer, runtime, func() (kernelNetworkDriver, error) { return openLinuxNetworkKernel(bearer, false) })
}

// Only this constructor carries WDS origin verification into the network
// lease. The structural Runtime constructor remains private for isolated tests.
func prepareProvenKernelNetwork(ctx context.Context, proof *RuntimeProof) (*kernelNetworkLease, error) {
	return prepareProvenKernelNetworkWith(ctx, proof, func() (kernelNetworkDriver, error) {
		if proof == nil || proof.source == nil {
			return nil, ErrRuntime
		}
		return openLinuxNetworkKernel(proof.source.lease, false)
	})
}
func prepareProvenKernelNetworkWith(ctx context.Context, proof *RuntimeProof, open func() (kernelNetworkDriver, error)) (*kernelNetworkLease, error) {
	if proof == nil || proof.source == nil {
		return nil, ErrRuntime
	}
	if err := proof.Verify(ctx); err != nil {
		return nil, err
	}
	n, err := prepareKernelNetworkWith(ctx, proof.source.lease, proof.Runtime(), open)
	if err != nil {
		return nil, err
	}
	err = n.locked(ctx, func(worker context.Context) error { n.proof = proof; return n.verify(worker) })
	if err != nil {
		// Retained in the claim until the observer finishes; canceled callers
		// must not block on a second round of network cleanup.
		go func() {
			cleanup, finish := context.WithTimeout(context.Background(), 15*time.Second)
			defer finish()
			_ = n.Close(cleanup)
		}()
		return nil, err
	}
	return n, nil
}
func prepareKernelNetworkWith(ctx context.Context, bearer *BearerLease, runtime Runtime, open func() (kernelNetworkDriver, error)) (*kernelNetworkLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if bearer == nil || bearer.claim == nil || open == nil {
		return nil, ErrSelection
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if bearer.pool.authorize(ctx, bearer.selection) != nil {
		return nil, ErrDenied
	}
	if err := bearer.Verify(ctx); err != nil {
		return nil, err
	}
	bearer.claim.mu.Lock()
	apn := bearer.claim.record.Options.APN
	bearer.claim.mu.Unlock()
	endpoints, err := runtime.Endpoints(apn)
	if err != nil {
		return nil, err
	}
	if len(endpoints) > 8 {
		return nil, ErrPCSCF
	}
	for _, endpoint := range endpoints {
		if _, ok := configurationAddress(bearer.configuration, endpoint.Local); !ok {
			return nil, ErrPCSCF
		}
	}
	driver, err := open()
	if err != nil {
		return nil, ErrNetwork
	}
	name, index, namespace, anchor := driver.identity()
	seed := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, seed); err != nil {
		driver.close()
		return nil, ErrNetwork
	}
	record := &networkClaim{Token: hex.EncodeToString(seed), Phase: "preparing", Namespace: namespace, Interface: name, Index: index, Anchor: anchor, Configuration: bearerConfigurationKey(bearer.configuration)}
	for attempt := 0; attempt < 8; attempt++ {
		record.Table = int(0x40000000 | (binary.LittleEndian.Uint32(seed[:4]) & 0x0fffffff))
		if driver.tableFree(record.Table) == nil {
			break
		}
		if _, err := io.ReadFull(rand.Reader, seed); err != nil {
			driver.close()
			return nil, ErrNetwork
		}
		if attempt == 7 {
			driver.close()
			return nil, ErrNetworkConflict
		}
	}
	clear(seed)
	if err := driver.verify(ctx); err != nil {
		driver.close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		driver.close()
		return nil, err
	}
	if !validNetworkClaim(record) {
		driver.close()
		return nil, ErrSelection
	}
	n := &kernelNetworkLease{bearer: bearer, driver: driver, record: record, gate: make(chan struct{}, 1), signaling: append([]Endpoint(nil), endpoints...), media: make(map[string]Endpoint)}
	bearer.claim.mu.Lock()
	c := bearer.claim
	if c.record.Token != bearer.token || c.record.Stage != "active" || c.record.Network != nil || c.busy || c.record.SIMPending || c.record.SIMUncertain || c.record.RuntimePending || c.record.RuntimeUncertain || c.storageFailed {
		c.mu.Unlock()
		driver.close()
		return nil, ErrBusy
	}
	next := c.record
	next.Network = cloneNetworkClaim(record)
	// Before is needed to correlate Create only. Once active, the exact owned
	// bearer path is authoritative and its old inventory need not grow the WAL.
	next.Before = nil
	err = bearer.pool.saveLocked(ctx, bearer.selection.key, c, next)
	if err == nil {
		if c.networkHolds == nil {
			c.networkHolds = make(map[string]any)
		}
		c.networkHolds[record.Token] = n
	}
	c.mu.Unlock()
	if err != nil {
		driver.close()
		return nil, err
	}
	done := make(chan error)
	go func() {
		worker, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := n.install(worker, ctx)
		if err != nil {
			if !n.uncertain {
				cleanup, finish := context.WithTimeout(context.Background(), 15*time.Second)
				if n.cleanup(cleanup) != nil {
					n.markUncertain()
				}
				finish()
			}
		}
		select {
		case done <- err:
			n.gate <- struct{}{}
		case <-ctx.Done():
			if err == nil {
				cleanup, finish := context.WithTimeout(context.Background(), 15*time.Second)
				if n.cleanup(cleanup) != nil {
					n.markUncertain()
				}
				finish()
			}
			n.gate <- struct{}{}
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			return nil, err
		}
		return n, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func configurationAddress(configuration BearerConfiguration, local netip.Addr) (BearerAddress, bool) {
	for _, address := range configuration.Addresses {
		if address.Address.Addr() == local {
			return address, true
		}
	}
	return BearerAddress{}, false
}
func (n *kernelNetworkLease) parent(ctx context.Context) error {
	c := n.bearer.claim
	c.mu.Lock()
	r := c.record
	if r.RuntimePending && !r.RuntimeUncertain {
		c.mu.Unlock()
		return ErrBusy
	}
	valid := r.Token == n.bearer.token && r.Stage == "active" && !r.Uncertain && !r.SIMPending && !r.SIMUncertain && !r.RuntimePending && !r.RuntimeUncertain && r.Pending == "" && !c.storageFailed && !c.busy && r.Network != nil && r.Network.Token == n.record.Token
	c.mu.Unlock()
	if !valid {
		return ErrNetwork
	}
	current, err := n.bearer.pool.owned(ctx, n.bearer.selection, c)
	if err != nil || !current.Connected || current.Suspended || !validConfiguration(current) || current.Interface != n.bearer.configuration.Interface || !reflect.DeepEqual(current.Addresses, n.bearer.configuration.Addresses) {
		return ErrNetwork
	}
	return n.driver.verify(ctx)
}
func (n *kernelNetworkLease) save(ctx context.Context, record *networkClaim) error {
	c := n.bearer.claim
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.record.Token != n.bearer.token || c.record.Network == nil || c.record.Network.Token != n.record.Token || c.storageFailed {
		return ErrJournal
	}
	if c.record.SIMPending || c.record.SIMUncertain || c.record.RuntimePending || c.record.RuntimeUncertain {
		return ErrBusy
	}
	if !validNetworkClaim(record) {
		return ErrJournal
	}
	next := c.record
	next.Network = cloneNetworkClaim(record)
	raw, err := json.Marshal(next)
	if err != nil || len(raw) > maxClaimBytes {
		return ErrJournal
	}
	if err := n.bearer.pool.saveLocked(ctx, n.bearer.selection.key, c, next); err != nil {
		return err
	}
	n.record = cloneNetworkClaim(record)
	return nil
}
func (n *kernelNetworkLease) markUncertain() {
	n.uncertain = true
	next := cloneNetworkClaim(n.record)
	next.Phase = "uncertain"
	_ = n.save(context.Background(), next)
}
func knownNetworkRejection(err error) bool {
	for _, code := range []error{unix.EEXIST, unix.EPERM, unix.EACCES, unix.EINVAL, unix.ENODEV, unix.EAFNOSUPPORT} {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}
func (n *kernelNetworkLease) add(ctx context.Context, op networkOperation) error {
	if n.bearer.pool.authorize(ctx, n.bearer.selection) != nil {
		return ErrDenied
	}
	if len(n.record.Operations) >= 20 {
		return ErrBusy
	}
	if err := n.parent(ctx); err != nil {
		return err
	}
	if hash, ready, err := n.driver.observe(op); err == nil {
		if op.Kind != "address" || !ready {
			return ErrNetworkConflict
		}
		op.Hash = hash
		op.Owned = false
		next := cloneNetworkClaim(n.record)
		next.Operations = append(next.Operations, op)
		return n.save(ctx, next)
	} else if !errors.Is(err, ErrNetworkMissing) {
		return err
	}
	op.Owned = true
	next := cloneNetworkClaim(n.record)
	next.Pending = &op
	if err := n.save(ctx, next); err != nil {
		return err
	}
	if err := n.driver.apply(op); err != nil {
		if knownNetworkRejection(err) {
			next = cloneNetworkClaim(n.record)
			next.Pending = nil
			if n.save(context.Background(), next) != nil {
				n.markUncertain()
				return ErrJournal
			}
			return ErrNetworkConflict
		}
		n.markUncertain()
		return ErrUncertain
	}
	// The add is confirmed even if the following readback cannot prove its
	// complete snapshot. Persist that fact before reading again.
	next = cloneNetworkClaim(n.record)
	next.Pending = nil
	next.Operations = append(next.Operations, op)
	if n.save(context.Background(), next) != nil {
		n.markUncertain()
		return ErrJournal
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		hash, ready, err := n.driver.observe(op)
		if err != nil {
			n.markUncertain()
			return ErrUncertain
		}
		next = cloneNetworkClaim(n.record)
		next.Operations[len(next.Operations)-1].Hash = hash
		if n.save(context.Background(), next) != nil {
			n.markUncertain()
			return ErrJournal
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return ErrNetworkNotReady
		case <-time.After(25 * time.Millisecond):
		}
	}
}
func (n *kernelNetworkLease) routeOperation(endpoint Endpoint, media bool) (networkOperation, error) {
	address, ok := configurationAddress(n.bearer.configuration, endpoint.Local)
	if !ok || !usable(endpoint.Remote) || endpoint.Local == endpoint.Remote || endpoint.Local.Is4() != endpoint.Remote.Is4() {
		return networkOperation{}, ErrParameters
	}
	if address.Gateway.IsValid() && address.Gateway == endpoint.Local {
		return networkOperation{}, ErrParameters
	}
	return networkOperation{Kind: "route", Local: address.Address, Remote: endpoint.Remote, Gateway: address.Gateway, MTU: address.MTU, Media: media}, nil
}
func (n *kernelNetworkLease) install(ctx context.Context, caller context.Context) error {
	if err := n.parent(ctx); err != nil {
		return err
	}
	for _, address := range n.bearer.configuration.Addresses {
		if err := caller.Err(); err != nil {
			return err
		}
		if err := n.add(ctx, networkOperation{Kind: "address", Local: address.Address}); err != nil {
			return err
		}
	}
	for _, address := range n.bearer.configuration.Addresses {
		if err := caller.Err(); err != nil {
			return err
		}
		if err := n.add(ctx, networkOperation{Kind: "rule", Local: address.Address}); err != nil {
			return err
		}
	}
	for _, endpoint := range n.signaling {
		if err := caller.Err(); err != nil {
			return err
		}
		exists := false
		for _, op := range n.record.Operations {
			if op.Kind == "route" && op.Remote == endpoint.Remote {
				if op.Local.Addr() != endpoint.Local {
					return ErrNetworkConflict
				}
				exists = true
			}
		}
		if exists {
			continue
		}
		op, err := n.routeOperation(endpoint, false)
		if err != nil {
			return err
		}
		if err := n.add(ctx, op); err != nil {
			return err
		}
	}
	if err := caller.Err(); err != nil {
		return err
	}
	if err := n.verify(ctx); err != nil {
		return err
	}
	next := cloneNetworkClaim(n.record)
	next.Phase = "active"
	return n.save(ctx, next)
}
func (n *kernelNetworkLease) verify(ctx context.Context) error {
	if n.bearer.pool.authorize(ctx, n.bearer.selection) != nil {
		return ErrDenied
	}
	if n.closed || n.uncertain || n.record.Pending != nil {
		return ErrUncertain
	}
	if n.proof != nil {
		if err := n.proof.Verify(ctx); err != nil {
			return err
		}
	}
	return n.verifySnapshot(ctx)
}

// Only PreparedAccess may pair this local check with the same completed NAS/WDS
// observation. General socket/media callers still use verify and fresh WDS.
func (n *kernelNetworkLease) verifySnapshot(ctx context.Context) error {
	if n.bearer.pool.authorize(ctx, n.bearer.selection) != nil {
		return ErrDenied
	}
	if n.closed || n.uncertain || n.record.Pending != nil {
		return ErrUncertain
	}
	if err := n.parent(ctx); err != nil {
		return err
	}
	for _, op := range n.record.Operations {
		hash, ready, err := n.driver.observe(op)
		if err != nil || !ready || hash != op.Hash {
			return ErrNetwork
		}
	}
	for _, endpoint := range n.signaling {
		if n.lookupEndpoint(endpoint) != nil {
			return ErrNetwork
		}
	}
	if n.record.Security != nil {
		if n.security == nil {
			return ErrSecurityScope
		}
		return n.security.verifyHeld(ctx)
	}
	return nil
}
func (n *kernelNetworkLease) locked(ctx context.Context, action func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-n.gate:
	}
	if err := ctx.Err(); err != nil {
		n.gate <- struct{}{}
		return err
	}
	done := make(chan error)
	go func() {
		worker, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := action(worker)
		select {
		case done <- err:
		case <-ctx.Done():
		}
		n.gate <- struct{}{}
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (n *kernelNetworkLease) Verify(ctx context.Context) error { return n.locked(ctx, n.verify) }
func (n *kernelNetworkLease) cleanup(ctx context.Context) error {
	if n.closed {
		return nil
	}
	// Protected I/O and all owned XFRM/TC resources must stop first. A parent
	// close may not remove the data plane underneath retained security state.
	if n.record.Security != nil {
		return ErrBusy
	}
	if n.uncertain || n.record.Pending != nil {
		return ErrUncertain
	}
	if err := n.parent(ctx); err != nil {
		return err
	}
	next := cloneNetworkClaim(n.record)
	next.Phase = "cleaning"
	if err := n.save(ctx, next); err != nil {
		return err
	}
	for len(n.record.Operations) > 0 {
		index := len(n.record.Operations) - 1
		op := n.record.Operations[index]
		if op.Owned {
			if err := n.parent(ctx); err != nil {
				return err
			}
			hash, _, err := n.driver.observe(op)
			if err != nil && !errors.Is(err, ErrNetworkMissing) {
				return err
			}
			if err == nil {
				if op.Hash == "" || hash != op.Hash {
					return ErrNetworkConflict
				}
				pending := op
				pending.Kind = "remove-" + op.Kind
				next = cloneNetworkClaim(n.record)
				next.Pending = &pending
				if err := n.save(ctx, next); err != nil {
					return err
				}
				if err := n.driver.remove(op); err != nil {
					n.markUncertain()
					return ErrUncertain
				}
			}
		}
		next = cloneNetworkClaim(n.record)
		next.Pending = nil
		next.Operations = next.Operations[:index]
		if err := n.save(ctx, next); err != nil {
			return err
		}
	}
	c := n.bearer.claim
	c.mu.Lock()
	if c.record.Token != n.bearer.token || c.record.Network == nil || c.record.Network.Token != n.record.Token {
		c.mu.Unlock()
		return ErrJournal
	}
	r := c.record
	r.Network = nil
	err := n.bearer.pool.saveLocked(ctx, n.bearer.selection.key, c, r)
	if err == nil {
		delete(c.networkHolds, n.record.Token)
	}
	c.mu.Unlock()
	if err != nil {
		return err
	}
	n.driver.close()
	n.closed = true
	return nil
}
func (n *kernelNetworkLease) Close(ctx context.Context) error {
	return n.locked(ctx, func(worker context.Context) error {
		err := n.cleanup(worker)
		if err != nil && !errors.Is(err, ErrBusy) {
			n.markUncertain()
		}
		return err
	})
}

// BindSocket is used before connect/listen. The bound interface plus the
// source+oif policy rule prevents modem traffic from taking a VPN/default path.
func (n *kernelNetworkLease) BindSocket(ctx context.Context, network, address string, raw syscall.RawConn) error {
	switch network {
	case "tcp", "tcp4", "tcp6", "udp", "udp4", "udp6":
	default:
		return ErrParameters
	}
	return n.locked(ctx, func(worker context.Context) error {
		if err := n.verify(worker); err != nil {
			return err
		}
		target, err := netip.ParseAddrPort(address)
		if err != nil || !usable(target.Addr()) || strings.HasSuffix(network, "4") && !target.Addr().Is4() || strings.HasSuffix(network, "6") && !target.Addr().Is6() {
			return ErrParameters
		}
		allowed := false
		if _, local := configurationAddress(n.bearer.configuration, target.Addr()); local {
			allowed = true
		}
		for _, endpoint := range n.signaling {
			if endpoint.Remote == target.Addr() {
				allowed = true
			}
		}
		for _, endpoint := range n.media {
			if endpoint.Remote == target.Addr() {
				allowed = true
			}
		}
		if !allowed || raw == nil {
			return ErrParameters
		}
		if err := n.driver.bindSocket(raw, target.Addr().Is4(), strings.HasPrefix(network, "tcp")); err != nil {
			return err
		}
		if n.security != nil {
			return n.security.markSocket(raw)
		}
		return nil
	})
}

type mediaRouteReference struct {
	network *kernelNetworkLease
	id      string
}

var _ vowifi.CellularIMSNetwork = (*kernelNetworkLease)(nil)

// This exposes only the private lease's network role, not Established IMS
// evidence. Serving-cell proof and application mode ownership remain required.
func (n *kernelNetworkLease) AcquireMediaRoute(ctx context.Context, local, remote netip.Addr) (vowifi.CellularMediaRoute, error) {
	reference, err := n.AddMedia(ctx, Endpoint{Local: local, Remote: remote})
	if err != nil {
		return nil, err
	}
	return reference, nil
}

func (r *mediaRouteReference) Close(ctx context.Context) error {
	if r == nil || r.network == nil {
		return nil
	}
	return r.network.locked(ctx, func(worker context.Context) error {
		if r.network.closed {
			delete(r.network.media, r.id)
			return nil
		}
		if _, exists := r.network.media[r.id]; !exists {
			return nil
		}
		return r.network.mutate(worker, func() error { return r.network.removeMedia(worker, r.id) })
	})
}
func (n *kernelNetworkLease) AddMedia(ctx context.Context, endpoint Endpoint) (*mediaRouteReference, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.gate:
	}
	if err := ctx.Err(); err != nil {
		n.gate <- struct{}{}
		return nil, err
	}
	type outcome struct {
		reference *mediaRouteReference
		err       error
	}
	done := make(chan outcome)
	go func() {
		worker, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out := outcome{}
		if err := ctx.Err(); err != nil {
			out.err = err
		} else {
			out.err = n.mutate(worker, func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				var err error
				out.reference, err = n.addMedia(worker, endpoint)
				return err
			})
		}
		select {
		case done <- out:
		case <-ctx.Done():
			if out.reference != nil {
				cleanup, finish := context.WithTimeout(context.Background(), 15*time.Second)
				if n.mutate(cleanup, func() error { return n.removeMedia(cleanup, out.reference.id) }) != nil {
					n.markUncertain()
				}
				finish()
			}
		}
		n.gate <- struct{}{}
	}()
	select {
	case out := <-done:
		return out.reference, out.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Persist a SIM exclusion phase before network mutation. The CAS and SIM gate
// share claim.mu; an AKA cannot slip between verification and the intent write.
func (n *kernelNetworkLease) mutate(ctx context.Context, action func() error) error {
	if err := n.verify(ctx); err != nil {
		return err
	}
	next := cloneNetworkClaim(n.record)
	next.Phase = "preparing"
	if err := n.save(ctx, next); err != nil {
		return err
	}
	err := action()
	if !n.uncertain {
		next = cloneNetworkClaim(n.record)
		next.Phase = "active"
		if n.save(ctx, next) != nil {
			n.markUncertain()
			return ErrJournal
		}
	}
	return err
}
func (n *kernelNetworkLease) addMedia(ctx context.Context, endpoint Endpoint) (*mediaRouteReference, error) {
	if len(n.media) >= 32 {
		return nil, ErrBusy
	}
	op, err := n.routeOperation(endpoint, true)
	if err != nil {
		return nil, err
	}
	existing := false
	mediaRoutes := 0
	for _, route := range n.record.Operations {
		if route.Kind != "route" {
			continue
		}
		if route.Media {
			mediaRoutes++
		}
		if route.Remote == endpoint.Remote {
			if route.Local.Addr() != endpoint.Local {
				return nil, ErrNetworkConflict
			}
			existing = true
		}
	}
	if !existing {
		if mediaRoutes >= 8 {
			return nil, ErrBusy
		}
		if err := n.add(ctx, op); err != nil {
			return nil, err
		}
	}
	if n.lookupEndpoint(endpoint) != nil {
		n.markUncertain()
		return nil, ErrNetwork
	}
	seed := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, seed); err != nil {
		n.markUncertain()
		return nil, ErrNetwork
	}
	id := hex.EncodeToString(seed)
	clear(seed)
	n.media[id] = endpoint
	return &mediaRouteReference{network: n, id: id}, nil
}
func (n *kernelNetworkLease) removeMedia(ctx context.Context, id string) error {
	endpoint, exists := n.media[id]
	if !exists {
		return nil
	}
	if n.closed || n.uncertain || n.record.Pending != nil {
		return ErrUncertain
	}
	for other, value := range n.media {
		if other != id && value.Remote == endpoint.Remote {
			delete(n.media, id)
			return nil
		}
	}
	for index, op := range n.record.Operations {
		if op.Kind != "route" || op.Remote != endpoint.Remote {
			continue
		}
		if !op.Media {
			delete(n.media, id)
			return nil
		}
		if err := n.parent(ctx); err != nil {
			return err
		}
		hash, _, err := n.driver.observe(op)
		if err != nil || hash != op.Hash {
			n.markUncertain()
			return ErrUncertain
		}
		pending := op
		pending.Kind = "remove-route"
		next := cloneNetworkClaim(n.record)
		next.Pending = &pending
		if n.save(ctx, next) != nil {
			n.markUncertain()
			return ErrJournal
		}
		if n.driver.remove(op) != nil {
			n.markUncertain()
			return ErrUncertain
		}
		next = cloneNetworkClaim(n.record)
		next.Pending = nil
		next.Operations = append(next.Operations[:index], next.Operations[index+1:]...)
		if n.save(context.Background(), next) != nil {
			n.markUncertain()
			return ErrJournal
		}
		delete(n.media, id)
		return nil
	}
	return ErrNetwork
}
