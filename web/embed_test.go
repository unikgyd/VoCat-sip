package web

import (
	"io/fs"
	"testing"
)

func TestEmbeddedDistributionContainsIndex(t *testing.T) {
	index, err := fs.ReadFile(Dist, "index.html")
	if err != nil {
		t.Fatalf("read embedded index.html: %v", err)
	}
	if len(index) == 0 {
		t.Fatal("embedded index.html is empty")
	}
}

func TestEmbeddedDistributionContainsPhoneWorklet(t *testing.T) {
	processor, err := fs.ReadFile(Dist, "phone-pcm.js")
	if err != nil || len(processor) == 0 {
		t.Fatalf("phone AudioWorklet missing from release: %v", err)
	}
}
