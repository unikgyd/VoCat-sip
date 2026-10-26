//go:build linux

package hostims

import (
	"context"
	"net/netip"
	"syscall"

	"vocat/internal/vowifi"
	"vocat/internal/vowifi/imssec"
)

// PrepareAccess composes one already owned IMS PDN with its exact card, NAS/WDS
// provenance, host network and security owner. It never enables RF, changes the
// module's IMS policy, or sends REGISTER. A non-nil owner on error must be kept
// and closed/retried; its bearer claim independently roots unfinished cleanup.
func PrepareAccess(ctx context.Context, lease *BearerLease) (*PreparedAccess, error) {
	sim, err := NewCellularSIMProvider(lease)
	if err != nil {
		return nil, err
	}
	nas, err := NewCellularNASServingCellSource(lease)
	if err != nil {
		return nil, err
	}
	return prepareAccessWith(ctx, lease, accessDependencies{sim: sim,
		observe: func(query context.Context) (accessSample, error) {
			proof, err := nas.Observe(query)
			if err != nil {
				return accessSample{}, err
			}
			runtime, err := proof.RuntimeProof()
			if err != nil {
				return accessSample{}, err
			}
			start, end := proof.QueryWindow()
			return accessSample{runtime: runtime, cell: proof.Sample(), start: start, end: end}, nil
		}, backend: &kernelPreparedAccess{lease: lease}})
}

type kernelPreparedAccess struct {
	lease       *BearerLease
	network     *kernelNetworkLease
	security    *kernelSecurityLease
	fingerprint string
	// Private injection points are only for synthetic source/kernel fixtures.
	openNetwork       func() (kernelNetworkDriver, error)
	securityDirectory string
	openSecurity      kernelSecurityFactory
}

func (b *kernelPreparedAccess) Prepare(ctx context.Context, sample accessSample) error {
	if b.network != nil || !b.sampleMatchesLease(sample) {
		return ErrRuntime
	}
	b.fingerprint = sample.runtime.fingerprint
	var err error
	if b.openNetwork == nil {
		b.network, err = prepareProvenKernelNetwork(ctx, sample.runtime)
	} else {
		b.network, err = prepareProvenKernelNetworkWith(ctx, sample.runtime, b.openNetwork)
	}
	if err != nil {
		return err
	}
	if b.openSecurity == nil {
		b.security, err = prepareKernelSecurity(ctx, b.network)
	} else {
		b.security, err = prepareKernelSecurityWith(ctx, b.network, b.securityDirectory, b.openSecurity)
	}
	if err != nil {
		return err
	}
	return b.Verify(ctx, sample)
}

func (b *kernelPreparedAccess) sampleMatchesLease(sample accessSample) bool {
	return b != nil && b.lease != nil && sample.runtime != nil && sample.runtime.source != nil &&
		sample.runtime.source.lease == b.lease && hashString(sample.runtime.fingerprint)
}

func (b *kernelPreparedAccess) Verify(ctx context.Context, sample accessSample) error {
	if !b.sampleMatchesLease(sample) || b.network == nil || b.security == nil || sample.runtime.fingerprint != b.fingerprint {
		return ErrChanged
	}
	// The caller just obtained this NAS observation and its exact clean WDS
	// result; do not add another full NAS/WDS transaction inside a 3s hook.
	return b.network.locked(ctx, b.network.verifySnapshot)
}

func (b *kernelPreparedAccess) Close(ctx context.Context) error {
	if b.network == nil && b.lease != nil && b.lease.claim != nil {
		// A canceled constructor may not deliver n even though its independently
		// rooted worker still owns the exact in-memory network lease. This is
		// not inference/adoption from a matching kernel object or cold journal.
		c := b.lease.claim
		c.mu.Lock()
		for _, held := range c.networkHolds {
			if n, ok := held.(*kernelNetworkLease); ok && n.bearer == b.lease {
				b.network = n
				break
			}
		}
		c.mu.Unlock()
	}
	if b.network == nil {
		return nil
	}
	if err := b.network.closeSecurity(ctx); err != nil {
		return err
	}
	return b.network.Close(ctx)
}

func (b *kernelPreparedAccess) BindSocket(ctx context.Context, network, address string, raw syscall.RawConn) error {
	if b.security == nil {
		return ErrSecurityScope
	}
	return b.security.BindSocket(ctx, network, address, raw)
}
func (b *kernelPreparedAccess) AcquireMediaRoute(ctx context.Context, local, remote netip.Addr) (vowifi.CellularMediaRoute, error) {
	if b.security == nil {
		return nil, ErrSecurityScope
	}
	return b.security.AcquireMediaRoute(ctx, local, remote)
}
func (b *kernelPreparedAccess) BindProtectedSocket(ctx context.Context, transport string, binding imssec.SocketBinding, raw syscall.RawConn) error {
	if b.security == nil {
		return ErrSecurityScope
	}
	return b.security.BindProtectedSocket(ctx, transport, binding, raw)
}
func (b *kernelPreparedAccess) InstallIPSec(ctx context.Context, config imssec.Config) (imssec.Handle, error) {
	if b.security == nil {
		return nil, ErrSecurityScope
	}
	return b.security.InstallIPSec(ctx, config)
}
func (b *kernelPreparedAccess) RetainIPSec(handle imssec.Handle) {
	if b.security != nil {
		b.security.RetainIPSec(handle)
	}
}
