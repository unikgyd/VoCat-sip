//go:build !linux || (!amd64 && !arm64 && !386 && !arm) || vocat_static

package buildinfo

const RuntimeFlavor = "static"
const NativeMedia = false
