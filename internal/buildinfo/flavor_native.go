//go:build linux && (amd64 || arm64 || 386 || arm) && !vocat_static

package buildinfo

// RuntimeFlavor is deliberately compile-time metadata. Self-update must not
// turn a deliberately static/musl installation into a glibc-only binary.
const RuntimeFlavor = "glibc-amr"
const NativeMedia = true
