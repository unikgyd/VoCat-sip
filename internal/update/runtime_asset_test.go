package update

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestReleaseAssetVariantNeverCrossesStaticIntoGlibc(t *testing.T) {
	for _, arch := range []string{"amd64", "386", "arm64", "arm"} {
		base := assetNamesFor("linux", arch)
		if actual := releaseAssetNamesFor("linux", arch, "glibc-amr"); !reflect.DeepEqual(actual, base) {
			t.Fatal(actual, base)
		}
		static := releaseAssetNamesFor("linux", arch, "static")
		if len(static) != len(base) {
			t.Fatal(static)
		}
		for i, name := range static {
			if name != base[i]+"-static" {
				t.Fatal("unsafe cross-flavor fallback", static)
			}
		}
	}
	if len(releaseAssetNamesFor("linux", "amd64", "unknown")) != 0 {
		t.Fatal("unknown flavor guessed an executable")
	}
}
func TestReleaseAssetInstallerLibcDetectionFailsClosed(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash installer only runs on Linux hosts")
	}
	script, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(script), "runtime_asset_suffix() {")
	if start < 0 {
		t.Fatal("installer selection helper missing")
	}
	end := strings.Index(string(script)[start:], "\ndownload_and_verify() {")
	if end < 0 {
		t.Fatal("installer selection helper missing")
	}
	helper := string(script)[start : start+end]
	for _, tc := range []struct{ Body, Want string }{{"printf 'glibc 2.36\\n'", ""}, {"printf 'musl 1.2\\n'", "-static"}, {"return 0", "-static"}, {"return 127", "-static"}} {
		command := exec.Command("bash", "-s")
		command.Stdin = strings.NewReader("set -eu\ngetconf() { " + tc.Body + "; }\n" + helper + "\nruntime_asset_suffix\n")
		actual, err := command.Output()
		if err != nil || string(actual) != tc.Want {
			t.Fatalf("helper %q = %q %v", tc.Body, actual, err)
		}
	}
}
