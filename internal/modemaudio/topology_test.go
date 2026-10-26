//go:build linux

package modemaudio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"vocat/internal/modem"
)

type audioFixture struct {
	sys, dev string
	r        Resolver
}

func newAudioFixture(t *testing.T) *audioFixture {
	t.Helper()
	root := t.TempDir()
	f := &audioFixture{sys: filepath.Join(root, "sys"), dev: filepath.Join(root, "dev")}
	for _, p := range []string{filepath.Join(f.sys, "bus/usb/devices"), filepath.Join(f.sys, "class/sound"), filepath.Join(f.dev, "snd")} {
		if e := os.MkdirAll(p, 0700); e != nil {
			t.Fatal(e)
		}
	}
	// Synthetic ordinary files, never real device nodes. Keep real FileInfo
	// identities for SameFile; only the character-mode predicate is replaced.
	f.r = Resolver{SysRoot: f.sys, DevRoot: f.dev, isPCMNode: func(os.FileInfo) bool { return true }}
	return f
}
func (f *audioFixture) put(t *testing.T, path, value string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte(value), 0600); e != nil {
		t.Fatal(e)
	}
}
func (f *audioFixture) link(t *testing.T, target, path string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(target, path); e != nil {
		t.Fatal(e)
	}
}
func (f *audioFixture) usb(t *testing.T, name, generation string) modem.Candidate {
	t.Helper()
	root := filepath.Join(f.sys, "devices/pci/usb1", name)
	f.put(t, filepath.Join(root, "idVendor"), "2c7c")
	f.put(t, filepath.Join(root, "idProduct"), "0125")
	f.put(t, filepath.Join(root, "busnum"), "1")
	f.put(t, filepath.Join(root, "devnum"), generation)
	alias := filepath.Join(f.sys, "bus/usb/devices", name)
	if _, e := os.Lstat(alias); os.IsNotExist(e) {
		f.link(t, root, alias)
	}
	return modem.Candidate{ID: name, VendorID: "2c7c", ProductID: "0125", USBPath: alias, USBGeneration: "1:" + generation, ATPort: modem.Port{Path: "/synthetic/" + name}}
}
func (f *audioFixture) card(t *testing.T, c modem.Candidate, index int, capture, playback []int) {
	t.Helper()
	usb, e := filepath.EvalSymlinks(c.USBPath)
	if e != nil {
		t.Fatal(e)
	}
	card := filepath.Join(usb, filepath.Base(usb)+":1.6", "sound", fmt.Sprintf("card%d", index))
	if e := os.MkdirAll(card, 0700); e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(f.sys, "class/sound", fmt.Sprintf("card%d", index))
	if _, e := os.Lstat(alias); os.IsNotExist(e) {
		f.link(t, card, alias)
	}
	for i, devices := range [][]int{capture, playback} {
		direction := "c"
		if i == 1 {
			direction = "p"
		}
		for _, device := range devices {
			name := fmt.Sprintf("pcmC%dD%d%s", index, device, direction)
			path := filepath.Join(card, name)
			if e := os.MkdirAll(path, 0700); e != nil {
				t.Fatal(e)
			}
			alias := filepath.Join(f.sys, "class/sound", name)
			if _, e := os.Lstat(alias); os.IsNotExist(e) {
				f.link(t, path, alias)
			}
			f.put(t, filepath.Join(f.dev, "snd", name), "synthetic; never opened")
		}
	}
}
func (f *audioFixture) one(t *testing.T) (modem.Candidate, *Binding) {
	t.Helper()
	c := f.usb(t, "1-2", "5")
	f.card(t, c, 2, []int{0}, []int{1})
	b, e := f.r.Resolve(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	return c, b
}

func TestUSBAudioPairExactPhysicalDeviceNotNameOrFirstCard(t *testing.T) {
	f := newAudioFixture(t)
	first := f.usb(t, "1-2", "5")
	other := f.usb(t, "1-20", "6")
	f.card(t, first, 7, []int{2}, []int{3})
	f.card(t, other, 0, []int{0}, []int{0})
	b, e := f.r.Resolve(context.Background(), first)
	if e != nil {
		t.Fatal(e)
	}
	if b.Info() != (PairInfo{7, 2, 3}) {
		t.Fatal(b.Info())
	}
	if e = b.Validate(context.Background()); e != nil {
		t.Fatal(e)
	}
	b, e = f.r.Resolve(context.Background(), other)
	if e != nil || b.Info() != (PairInfo{0, 0, 0}) {
		t.Fatal(b, e)
	}
}
func TestUSBAudioPairRefusesAmbiguityAndHalfDuplex(t *testing.T) {
	for _, tc := range []struct {
		name                string
		captures, playbacks []int
		second              bool
		want                error
	}{
		{"capture", []int{0, 1}, []int{0}, false, ErrAmbiguous}, {"playback", []int{0}, []int{0, 1}, false, ErrAmbiguous}, {"second-card", []int{0}, []int{0}, true, ErrAmbiguous}, {"no-input", nil, []int{0}, false, ErrUnavailable}, {"no-output", []int{0}, nil, false, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAudioFixture(t)
			c := f.usb(t, "1-2", "5")
			f.card(t, c, 2, tc.captures, tc.playbacks)
			if tc.second {
				f.card(t, c, 3, []int{0}, []int{0})
			}
			if _, e := f.r.Resolve(context.Background(), c); !errors.Is(e, tc.want) {
				t.Fatal(e)
			}
		})
	}
}
func TestUSBAudioPairRejectsForgedOrMissingIdentity(t *testing.T) {
	f := newAudioFixture(t)
	c, _ := f.one(t)
	for _, change := range []func(*modem.Candidate){func(c *modem.Candidate) { c.USBGeneration = "" }, func(c *modem.Candidate) { c.USBGeneration = "1:8" }, func(c *modem.Candidate) { c.VendorID = "abcd" }, func(c *modem.Candidate) { c.USBPath = filepath.Join(f.sys, "devices/pci") }, func(c *modem.Candidate) { c.ProductID = "../x" }} {
		copy := c
		change(&copy)
		if _, e := f.r.Resolve(context.Background(), copy); e == nil {
			t.Fatal("unbound candidate accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := f.r.Resolve(ctx, c); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestUSBAudioBindingRejectsReenumerationAndNodeReuse(t *testing.T) {
	for _, kind := range []string{"generation", "usb-inode", "node-inode", "new-ambiguity"} {
		t.Run(kind, func(t *testing.T) {
			f := newAudioFixture(t)
			c, b := f.one(t)
			usb, _ := filepath.EvalSymlinks(c.USBPath)
			switch kind {
			case "generation":
				f.put(t, filepath.Join(usb, "devnum"), "6")
			case "usb-inode":
				if e := os.Rename(usb, usb+".old"); e != nil {
					t.Fatal(e)
				}
				c = f.usb(t, "1-2", "5")
				f.card(t, c, 2, []int{0}, []int{1})
			case "node-inode":
				if e := os.Rename(b.capture, b.capture+".old"); e != nil {
					t.Fatal(e)
				}
				f.put(t, b.capture, "replacement")
			case "new-ambiguity":
				f.card(t, c, 2, []int{3}, nil)
			}
			if e := b.Validate(context.Background()); !errors.Is(e, ErrChanged) {
				t.Fatal(e)
			}
		})
	}
}
func TestUSBAudioPairRequiresRealCharacterNodesByDefault(t *testing.T) {
	f := newAudioFixture(t)
	c, _ := f.one(t)
	r := f.r
	r.isPCMNode = nil
	if _, e := r.Resolve(context.Background(), c); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
}
func TestUSBAudioBindingCannotBeManufactured(t *testing.T) {
	if e := (&Binding{}).Validate(context.Background()); !errors.Is(e, ErrChanged) {
		t.Fatal(e)
	}
}
