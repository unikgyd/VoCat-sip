package update

// A static installation must never silently upgrade into a glibc-dependent
// executable. Missing static assets fail closed, without a cross-flavor fallback.
func releaseAssetNamesFor(goos, goarch, flavor string) []string {
	names := assetNamesFor(goos, goarch)
	if goos != "linux" {
		return names
	}
	switch flavor {
	case "glibc-amr":
		return names
	case "static":
		for i := range names {
			names[i] += "-static"
		}
		return names
	default:
		return nil
	}
}
