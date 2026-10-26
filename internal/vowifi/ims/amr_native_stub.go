//go:build !linux || (!amd64 && !arm64 && !386 && !arm) || vocat_static

package ims

func openNativeAMR() (amrCodec, error) { return nil, errAMRUnavailable }
