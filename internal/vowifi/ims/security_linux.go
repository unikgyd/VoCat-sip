//go:build linux

package ims

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type linuxIPSecInstaller struct {
	ipCommand string
}

func defaultIPSecInstaller() IPSecSAInstaller {
	return linuxIPSecInstaller{ipCommand: "ip"}
}

type linuxIPSecHandle struct {
	mu        sync.Mutex
	ipCommand string
	// Only acknowledged additions acquire a corresponding deletion. Never
	// infer ownership from the requested SA set after an add fails (EEXIST).
	pending []xfrmOperation
}

func (installer linuxIPSecInstaller) Install(ctx context.Context, config IPSecSAConfig) (IPSecSAHandle, error) {
	command := installer.ipCommand
	if command == "" {
		command = "ip"
	}
	if _, err := exec.LookPath(command); err != nil {
		return nil, errors.New("ims: Linux iproute2 is required for ipsec-3gpp")
	}
	install, err := buildXFRMInstallPlan(config)
	if err != nil {
		return nil, err
	}
	handle := &linuxIPSecHandle{
		ipCommand: command,
	}
	cleanup := buildXFRMCleanupPlan(config)
	for index, operation := range install {
		if err := runIPCommand(ctx, command, operation); err != nil {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			cleanupErr := handle.cleanup(cleanupContext)
			cancel()
			return nil, errors.Join(fmt.Errorf("%w: %v", ErrIPSecInstall, err), cleanupErr)
		}
		handle.pending = append(handle.pending, cleanup[len(cleanup)-1-index])
	}
	return handle, nil
}

func (handle *linuxIPSecHandle) Close(ctx context.Context) error {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	return handle.cleanup(ctx)
}

func (handle *linuxIPSecHandle) cleanup(ctx context.Context) error {
	var cleanupErrors []error
	var remaining []xfrmOperation
	for index := len(handle.pending) - 1; index >= 0; index-- {
		operation := handle.pending[index]
		if err := runIPCommand(ctx, handle.ipCommand, operation); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			remaining = append(remaining, operation)
		}
	}
	// Preserve installation order so a retry again deletes in reverse order;
	// already deleted objects must never be targeted a second time.
	for left, right := 0, len(remaining)-1; left < right; left, right = left+1, right-1 {
		remaining[left], remaining[right] = remaining[right], remaining[left]
	}
	handle.pending = remaining
	return errors.Join(cleanupErrors...)
}

func runIPCommand(ctx context.Context, command string, operation xfrmOperation) error {
	output, err := exec.CommandContext(ctx, command, operation.arguments...).CombinedOutput()
	if err == nil {
		return nil
	}
	message := strings.ToLower(string(output))
	if strings.Contains(message, "protocol not supported") ||
		strings.Contains(message, "operation not supported") {
		return fmt.Errorf(
			"%s: host kernel lacks XFRM/IPsec support; install matching kmod-ipsec and kmod-ipsec4/6 (OpenWrt), or enable CONFIG_XFRM_USER and ESP in the kernel",
			operation.description,
		)
	}
	// iproute2 can echo the rejected arguments, including CK/IK-derived keys.
	// Do not expose raw stdout/stderr or command strings through diagnostics.
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", operation.description, ctx.Err())
	}
	return fmt.Errorf("%s: iproute2 operation failed", operation.description)
}
