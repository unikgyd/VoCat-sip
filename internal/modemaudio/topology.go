// Package modemaudio provides physically bound USB modem PCM, never a default
// desktop microphone/speaker. A discovered pair alone is not a working call.
package modemaudio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"vocat/internal/modem"
)

var (
	ErrUnavailable = errors.New("modem audio is unavailable")
	ErrAmbiguous   = errors.New("modem audio endpoints are not unique")
	ErrChanged     = errors.New("modem audio binding changed")
	ErrBusy        = errors.New("modem audio is already owned")
	ErrClosed      = errors.New("modem audio is closed")
	ErrNotStarted  = errors.New("modem audio is not started")
	ErrFormat      = errors.New("modem audio requires mono 8 kHz signed 16-bit PCM")
	errAgain       = errors.New("PCM would block")
)

type PairInfo struct {
	Card           int `json:"card"`
	CaptureDevice  int `json:"capture_device"`
	PlaybackDevice int `json:"playback_device"`
}

type Info struct {
	Pairing          string    `json:"pairing"`
	Pair             *PairInfo `json:"pair,omitempty"`
	BackendAvailable bool      `json:"backend_available"`
	// Metadata inspection never claims an opened, owned, active call bridge.
	VoiceBridgeReady bool `json:"voice_bridge_ready"`
	SampleRate       int  `json:"sample_rate"`
	Channels         int  `json:"channels"`
}

type Inspector interface {
	Inspect(context.Context, modem.Candidate) (Info, error)
}

// Resolver has no audio I/O. Alternate roots are an in-process test seam, not
// accepted from HTTP/configuration or used to choose an arbitrary PCM name.
type Resolver struct {
	SysRoot   string
	DevRoot   string
	isPCMNode func(os.FileInfo) bool
}

type pinnedPath struct {
	path string
	info os.FileInfo
}

// Binding is intentionally opaque. JSON/client input cannot manufacture or
// mutate a verified card index, generation or device path for Open.
type Binding struct {
	resolver          Resolver
	candidate         modem.Candidate
	usb               string
	generation        string
	pair              PairInfo
	capture, playback string
	pinned            []pinnedPath
}

func (b *Binding) Info() PairInfo { return b.pair }

var cardName = regexp.MustCompile(`^card(0|[1-9][0-9]{0,3})$`)
var pcmName = regexp.MustCompile(`^pcmC(0|[1-9][0-9]{0,3})D(0|[1-9][0-9]{0,3})([cp])$`)
var hexID = regexp.MustCompile(`^[0-9a-f]{4}$`)

func beneath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (r Resolver) roots() (Resolver, error) {
	if r.SysRoot == "" {
		r.SysRoot = "/sys"
	}
	if r.DevRoot == "" {
		r.DevRoot = "/dev"
	}
	var err error
	r.SysRoot, err = filepath.EvalSymlinks(r.SysRoot)
	if err != nil {
		return r, ErrUnavailable
	}
	r.DevRoot, err = filepath.EvalSymlinks(r.DevRoot)
	if err != nil {
		return r, ErrUnavailable
	}
	if !filepath.IsAbs(r.SysRoot) || !filepath.IsAbs(r.DevRoot) {
		return r, ErrUnavailable
	}
	if r.isPCMNode == nil {
		r.isPCMNode = func(info os.FileInfo) bool { return info.Mode()&os.ModeCharDevice != 0 }
	}
	return r, nil
}

func readValue(path string) (string, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return "", e
	}
	return strings.TrimSpace(string(b)), nil
}

func usbGeneration(path string) (string, error) {
	values := make([]string, 2)
	for i, name := range []string{"busnum", "devnum"} {
		v, e := readValue(filepath.Join(path, name))
		n, parse := strconv.Atoi(v)
		if e != nil || parse != nil || n < 1 || n > 65535 {
			return "", ErrUnavailable
		}
		values[i] = strconv.Itoa(n)
	}
	return strings.Join(values, ":"), nil
}

func nearestUSB(root, path string) (string, error) {
	for beneath(root, path) && path != root {
		_, a := os.Stat(filepath.Join(path, "idVendor"))
		_, b := os.Stat(filepath.Join(path, "idProduct"))
		if a == nil && b == nil {
			return path, nil
		}
		if !os.IsNotExist(a) || !os.IsNotExist(b) {
			return "", ErrUnavailable
		}
		path = filepath.Dir(path)
	}
	return "", ErrUnavailable
}

func (r Resolver) Resolve(ctx context.Context, c modem.Candidate) (*Binding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := r.roots()
	if err != nil {
		return nil, err
	}
	if c.USBPath == "" || c.USBGeneration == "" || !hexID.MatchString(c.VendorID) || !hexID.MatchString(c.ProductID) {
		return nil, ErrUnavailable
	}
	// A PCIe/MHI path or a parent USB hub is not the selected USB function.
	link := filepath.Clean(c.USBPath)
	if filepath.Dir(link) != filepath.Join(r.SysRoot, "bus", "usb", "devices") {
		return nil, ErrUnavailable
	}
	usb, err := filepath.EvalSymlinks(link)
	if err != nil || !beneath(r.SysRoot, usb) {
		return nil, ErrUnavailable
	}
	usbInfo, err := os.Stat(usb)
	if err != nil {
		return nil, ErrUnavailable
	}
	vendor, a := readValue(filepath.Join(usb, "idVendor"))
	product, b := readValue(filepath.Join(usb, "idProduct"))
	generation, g := usbGeneration(usb)
	if a != nil || b != nil || g != nil || vendor != c.VendorID || product != c.ProductID || generation != c.USBGeneration {
		return nil, ErrChanged
	}
	entries, err := os.ReadDir(filepath.Join(r.SysRoot, "class", "sound"))
	if err != nil {
		return nil, ErrUnavailable
	}
	type card struct {
		index int
		path  string
	}
	var cards []card
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match := cardName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		path, e := filepath.EvalSymlinks(filepath.Join(r.SysRoot, "class", "sound", entry.Name()))
		if e != nil || !beneath(r.SysRoot, path) {
			continue
		}
		parent, e := nearestUSB(r.SysRoot, path)
		if e != nil || parent != usb {
			continue
		}
		index, _ := strconv.Atoi(match[1])
		cards = append(cards, card{index, path})
	}
	if len(cards) > 1 {
		return nil, ErrAmbiguous
	}
	if len(cards) != 1 {
		return nil, ErrUnavailable
	}
	selected := cards[0]
	type endpoint struct {
		device     int
		path, node string
	}
	var capture, playback []endpoint
	for _, entry := range entries {
		m := pcmName.FindStringSubmatch(entry.Name())
		if m == nil || m[1] != strconv.Itoa(selected.index) {
			continue
		}
		path, e := filepath.EvalSymlinks(filepath.Join(r.SysRoot, "class", "sound", entry.Name()))
		if e != nil || filepath.Dir(path) != selected.path {
			return nil, ErrChanged
		}
		parent, e := nearestUSB(r.SysRoot, path)
		if e != nil || parent != usb {
			return nil, ErrChanged
		}
		node := filepath.Join(r.DevRoot, "snd", entry.Name())
		resolved, e := filepath.EvalSymlinks(node)
		if e != nil || !beneath(r.DevRoot, resolved) {
			return nil, ErrUnavailable
		}
		info, e := os.Stat(node)
		if e != nil || !r.isPCMNode(info) {
			return nil, ErrUnavailable
		}
		device, _ := strconv.Atoi(m[2])
		point := endpoint{device, path, node}
		if m[3] == "c" {
			capture = append(capture, point)
		} else {
			playback = append(playback, point)
		}
	}
	if len(capture) > 1 || len(playback) > 1 {
		return nil, ErrAmbiguous
	}
	if len(capture) != 1 || len(playback) != 1 {
		return nil, ErrUnavailable
	}
	result := &Binding{resolver: r, candidate: c, usb: usb, generation: generation, pair: PairInfo{selected.index, capture[0].device, playback[0].device}, capture: capture[0].node, playback: playback[0].node}
	for _, path := range []string{usb, selected.path, capture[0].path, playback[0].path, capture[0].node, playback[0].node} {
		var info os.FileInfo
		info, err = os.Stat(path)
		if err != nil {
			return nil, ErrChanged
		}
		result.pinned = append(result.pinned, pinnedPath{path, info})
	}
	finalGeneration, err := usbGeneration(usb)
	if err != nil || finalGeneration != generation || !os.SameFile(usbInfo, result.pinned[0].info) {
		return nil, ErrChanged
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (b *Binding) Validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b == nil || b.usb == "" || len(b.pinned) != 6 {
		return ErrChanged
	}
	current, err := b.resolver.Resolve(ctx, b.candidate)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: selected endpoints are no longer unique/current", ErrChanged)
	}
	if current.usb != b.usb || current.generation != b.generation || current.pair != b.pair || current.capture != b.capture || current.playback != b.playback {
		return ErrChanged
	}
	for i, item := range b.pinned {
		if current.pinned[i].path != item.path || !os.SameFile(item.info, current.pinned[i].info) {
			return ErrChanged
		}
	}
	return nil
}
