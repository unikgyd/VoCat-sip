//go:build !windows

package vowifi

func imsPlatformNetworkFailure(error) bool { return false }
