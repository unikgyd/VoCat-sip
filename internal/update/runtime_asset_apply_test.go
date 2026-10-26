package update

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"vocat/internal/buildinfo"
)

// Exercise the actual authenticated-download/checksum/validate/replace path,
// using only synthetic scripts and a private HTTP peer. restart=false is vital.
func TestReleaseAssetApplyUsesBuildFlavorAndKeepsOtherFlavorUnavailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux release artifact regression")
	}
	expected := releaseAssetNamesFor("linux", runtime.GOARCH, buildinfo.RuntimeFlavor)[0]
	other := assetNamesFor("linux", runtime.GOARCH)[0]
	if !strings.HasSuffix(expected, "-static") {
		other += "-static"
	}
	payload := []byte("#!/bin/sh\nprintf 'vocat 99.0.0\\n'\n")
	hash := sha256.Sum256(payload)
	var correctRequests, wrongRequests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/correct":
			correctRequests.Add(1)
			_, _ = w.Write(payload)
		case "/wrong":
			wrongRequests.Add(1)
			http.Error(w, "wrong flavor", 500)
		case "/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%x  %s\n", hash, expected)
		default:
			http.NotFound(w, r)
		}
	}))
	defer peer.Close()
	directory := t.TempDir()
	target := filepath.Join(directory, "vocat")
	if err := os.WriteFile(target, []byte("old installed placeholder"), 0700); err != nil {
		t.Fatal(err)
	}
	release := &Release{TagName: "v99.0.0", Assets: []Asset{{Name: other, BrowserDownloadURL: peer.URL + "/wrong", Size: 1}, {Name: expected, BrowserDownloadURL: peer.URL + "/correct", Size: int64(len(payload))}, {Name: "SHA256SUMS", BrowserDownloadURL: peer.URL + "/SHA256SUMS"}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := applyUpdate(context.Background(), logger, Options{Target: target}, release, "99.0.0", false); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != string(payload) || correctRequests.Load() != 1 || wrongRequests.Load() != 0 {
		t.Fatal("wrong release variant installed")
	}
	previous, err := os.ReadFile(target + ".previous")
	if err != nil || string(previous) != "old installed placeholder" {
		t.Fatal("previous binary not retained")
	}
	// Removing this exact flavor must fail without touching the installed file
	// or falling back to a present but incompatible artifact.
	release.Assets = release.Assets[:1]
	if err := applyUpdate(context.Background(), logger, Options{Target: target}, release, "99.0.1", false); err == nil {
		t.Fatal("cross-flavor fallback accepted")
	}
	actual, _ = os.ReadFile(target)
	if string(actual) != string(payload) || wrongRequests.Load() != 0 {
		t.Fatal("missing flavor modified installation")
	}
}
