//go:build linux

package hostims

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

type topologyLinks map[int]netlink.Link

func (t topologyLinks) LinkByName(name string) (netlink.Link, error) {
	for _, link := range t {
		if link.Attrs().Name == name {
			return link, nil
		}
	}
	return nil, ErrQMIEndpoint
}
func (t topologyLinks) LinkByIndex(index int) (netlink.Link, error) {
	link, ok := t[index]
	if !ok {
		return nil, ErrQMIEndpoint
	}
	return link, nil
}
func (topologyLinks) SetSocketTimeout(time.Duration) error { return nil }
func (topologyLinks) Close()                               {}

func wdsTopologyFixture(t *testing.T) (*BearerLease, *qmiEndpointPin, string, topologyLinks) {
	t.Helper()
	selection, roots, target := endpointPinFixture(t)
	_, _, _, base := bearerFixture(t)
	lease := &BearerLease{selection: base, claim: &bearerClaim{record: claimRecord{}}, configuration: BearerConfiguration{Interface: "qmimux0"}}
	function := filepath.Dir(filepath.Dir(target))
	if err := os.MkdirAll(filepath.Join(roots.sys, "bus/usb"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(roots.sys, "bus/usb"), filepath.Join(function, "subsystem")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(function, "bInterfaceNumber"), []byte("04\n"), 0600); err != nil {
		t.Fatal(err)
	}
	links := topologyLinks{2: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 2, Name: "wwan0"}}, 3: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 3, Name: "qmimux0", ParentIndex: 2}}}
	for index, link := range links {
		path := filepath.Join(roots.sys, "devices/virtual/net", link.Attrs().Name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(roots.sys, "class/net"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(roots.sys, "class/net", link.Attrs().Name)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "ifindex"), []byte(strconv.Itoa(index)), 0600); err != nil {
			t.Fatal(err)
		}
		parent := link.Attrs().ParentIndex
		if parent == 0 {
			parent = index
		}
		if err := os.WriteFile(filepath.Join(path, "iflink"), []byte(strconv.Itoa(parent)), 0600); err != nil {
			t.Fatal(err)
		}
		if index == 2 {
			if err := os.Symlink(function, filepath.Join(path, "device")); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.Mkdir(filepath.Join(path, "qmap"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "qmap/mux_id"), []byte("0x0b\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	pin, err := pinQMIEndpoint(selection, roots)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pin.Close() })
	return lease, pin, roots.sys, links
}
func TestWDSTopologyUSBFunctionAndMuxArePinned(t *testing.T) {
	lease, pin, sys, links := wdsTopologyFixture(t)
	proof, err := pinWDSTopologyWith(lease, pin, sys, func(*os.File) (wdsLinkReader, error) { return links, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer proof.Close()
	if proof.Binding() != (wdsBinding{Endpoint: 2, Interface: 4, Mux: 11}) || !hashString(proof.Key()) {
		t.Fatal("wrong binding")
	}
	if err := proof.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proof.links[0].node.path, "qmap/mux_id"), []byte("0x0c"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := proof.Verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatal("changed mux accepted", err)
	}
}
func TestWDSTopologyRejectsUnprovenPhysicalMapping(t *testing.T) {
	for _, mode := range []string{"bad-interface", "bad-mux", "foreign-function", "wrong-ifindex", "wrong-parent", "native-not-usb"} {
		t.Run(mode, func(t *testing.T) {
			lease, pin, sys, links := wdsTopologyFixture(t)
			function := filepath.Dir(filepath.Dir(pin.charTarget))
			child := filepath.Join(sys, "devices/virtual/net/qmimux0")
			switch mode {
			case "bad-interface":
				if err := os.WriteFile(filepath.Join(function, "bInterfaceNumber"), []byte("zz"), 0600); err != nil {
					t.Fatal(err)
				}
			case "bad-mux":
				if err := os.WriteFile(filepath.Join(child, "qmap/mux_id"), []byte("0xff"), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-function":
				path := filepath.Join(sys, "devices/virtual/net/wwan0/device")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(function), path); err != nil {
					t.Fatal(err)
				}
			case "wrong-ifindex":
				if err := os.WriteFile(filepath.Join(child, "ifindex"), []byte("9"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-parent":
				links[3].Attrs().ParentIndex = 8
			case "native-not-usb":
				if err := os.Remove(filepath.Join(function, "bInterfaceNumber")); err != nil {
					t.Fatal(err)
				}
			}
			if proof, err := pinWDSTopologyWith(lease, pin, sys, func(*os.File) (wdsLinkReader, error) { return links, nil }); err == nil {
				proof.Close()
				t.Fatal("unproven endpoint accepted")
			}
		})
	}
}
func TestWDSTopologySinglePDNRequiresDirectUSBFunction(t *testing.T) {
	lease, pin, sys, links := wdsTopologyFixture(t)
	lease.claim.record.Options.SinglePDN = true
	lease.configuration.Interface = "wwan0"
	proof, err := pinWDSTopologyWith(lease, pin, sys, func(*os.File) (wdsLinkReader, error) { return links, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer proof.Close()
	if proof.Binding().Mux != 0 {
		t.Fatal("single PDN got a fabricated mux")
	}
}
func TestWDSTopologyRmnetRawDecoder(t *testing.T) {
	for _, mode := range []string{"valid", "wrong-index", "duplicate", "missing", "trailing", "range", "wrong-kind", "byte-order"} {
		t.Run(mode, func(t *testing.T) {
			mux := make([]byte, 2)
			binary.NativeEndian.PutUint16(mux, 11)
			if mode == "range" {
				binary.NativeEndian.PutUint16(mux, 255)
			}
			data := nl.NewRtAttr(0, nil)
			if mode != "missing" {
				kind := 1
				if mode == "byte-order" {
					kind |= 0x4000
				}
				data.AddChild(nl.NewRtAttr(kind, mux))
			}
			if mode == "duplicate" {
				data.AddChild(nl.NewRtAttr(1, mux))
			}
			kind := "rmnet\x00"
			if mode == "wrong-kind" {
				kind = "vlan\x00"
			}
			info := nl.NewRtAttr(0, nil)
			info.AddChild(nl.NewRtAttr(1, []byte(kind)))
			info.AddChild(nl.NewRtAttr(2, data.Serialize()[4:]))
			attr := nl.NewRtAttr(18, info.Serialize()[4:])
			raw := make([]byte, 16)
			binary.NativeEndian.PutUint32(raw[4:8], 3)
			raw = append(raw, attr.Serialize()...)
			index := 3
			if mode == "wrong-index" {
				index = 4
			}
			if mode == "trailing" {
				raw = append(raw, 1)
			}
			value, err := parseRmnetMux(raw, index)
			if mode == "valid" {
				if err != nil || value != 11 {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("malformed mux metadata accepted")
			}
		})
	}
}

func FuzzWDSTopologyRmnetMetadata(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, 16))
	raw := make([]byte, 16)
	binary.NativeEndian.PutUint32(raw[4:8], 3)
	f.Add(append(raw, 4, 0, 18, 0, 1))
	data := nl.NewRtAttr(0, nil)
	data.AddChild(nl.NewRtAttr(1, []byte{11, 0}))
	info := nl.NewRtAttr(0, nil)
	info.AddChild(nl.NewRtAttr(1, []byte("rmnet\x00")))
	info.AddChild(nl.NewRtAttr(2, data.Serialize()[4:]))
	f.Add(append(append([]byte(nil), raw...), nl.NewRtAttr(18, info.Serialize()[4:]).Serialize()...))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if value, err := parseRmnetMux(raw, 3); err == nil && (value < 1 || value > 254) {
			t.Fatal("invalid mux escaped")
		}
	})
}
