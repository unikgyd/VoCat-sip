//go:build !linux || (!amd64 && !arm64 && !386 && !arm) || vocat_static

package modemaudio

import "context"

func nativeAvailable() bool                             { return false }
func openNative(context.Context, *Binding) (pcm, error) { return nil, ErrUnavailable }
