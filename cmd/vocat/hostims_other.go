//go:build !linux

package main

import (
	"context"
	"sync"

	"vocat/internal/device"
	"vocat/internal/hostims"
	"vocat/internal/hostimsruntime"
	"vocat/internal/store"
)

type unsupportedHostIMSFactory struct {
	opts   hostIMSFactoryOptions
	mu     sync.Mutex
	closed bool
}

func hostIMSInventoryPhysicalID(store.Device, []device.Device) (string, error) {
	return "", errHostIMSFactory
}

func buildHostIMSFactory(ctx context.Context, opts hostIMSFactoryOptions) (hostIMSRuntimeFactory, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if opts.Store == nil || opts.Devices == nil || opts.IMSConfig == nil || opts.Authorize == nil {
		return nil, errHostIMSFactory
	}
	return &unsupportedHostIMSFactory{opts: opts}, nil
}

// Non-Linux registration stays inert so ordinary module/VoWiFi operation is
// not made fatal by an unavailable optional Host IMS platform capability.
func (f *unsupportedHostIMSFactory) Factory(ctx context.Context, id string) (*hostimsruntime.Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed || ctx.Err() != nil {
		return nil, errHostIMSFactory
	}
	config, err := f.opts.Store.Device(ctx, id)
	if err != nil {
		return nil, errHostIMSFactory
	}
	imsConfig, err := f.opts.IMSConfig(ctx, config)
	if err != nil {
		return nil, errHostIMSFactory
	}
	return hostimsruntime.NewRuntime(hostimsruntime.Config{DeviceID: id, IMS: imsConfig, Prepare: func(context.Context) (hostimsruntime.Access, error) { return nil, hostims.ErrHostAccessUnavailable }})
}
func (f *unsupportedHostIMSFactory) Close(context.Context) error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}
