//go:build linux && (amd64 || arm64 || 386 || arm) && !vocat_static

package modemaudio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"

	"github.com/ebitengine/purego"
)

type alsa struct {
	library                                           uintptr
	loadConfig                                        func(*uintptr, string, uintptr) int32
	deleteConfig                                      func(uintptr) int32
	openPCM                                           func(*uintptr, string, int32, int32, uintptr) int32
	closePCM, drop, prepare, pcmType, descriptorCount func(uintptr) int32
	setParams                                         func(uintptr, int32, int32, uint32, uint32, int32, uint32) int32
	swMalloc                                          func(*uintptr) int32
	swFree                                            func(uintptr)
	swCurrent, swApply                                func(uintptr, uintptr) int32
	swStartThreshold                                  func(uintptr, uintptr, uintptr) int32
	read, write                                       func(uintptr, *int16, uintptr) int // snd_pcm_sframes_t is C long, not fixed int64.
	wait                                              func(uintptr, int32) int32
	infoMalloc                                        func(*uintptr) int32
	infoFree                                          func(uintptr)
	info                                              func(uintptr, uintptr) int32
	infoCard, infoStream                              func(uintptr) int32
	infoDevice, infoSubdevice                         func(uintptr) uint32
	descriptors                                       func(uintptr, *pollDescriptor, uint32) int32
}
type pollDescriptor struct {
	FD              int32
	Events, Revents int16
}
type alsaPCM struct{ handle, config uintptr }
type alsaDuplex struct {
	api                     *alsa
	capture, playback       alsaPCM
	readFaults, writeFaults int
}

func loadALSA(names []string) (result *alsa, err error) {
	a := &alsa{}
	defer func() {
		if recover() != nil {
			if a.library != 0 {
				_ = purego.Dlclose(a.library)
			}
			result = nil
			err = ErrUnavailable
		}
	}()
	for _, name := range names {
		a.library, err = purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err == nil {
			break
		}
	}
	if a.library == 0 {
		return nil, ErrUnavailable
	}
	for _, symbol := range []struct {
		fn   any
		name string
	}{
		{&a.loadConfig, "snd_config_load_string"}, {&a.deleteConfig, "snd_config_delete"}, {&a.openPCM, "snd_pcm_open_lconf"}, {&a.closePCM, "snd_pcm_close"}, {&a.drop, "snd_pcm_drop"}, {&a.prepare, "snd_pcm_prepare"}, {&a.pcmType, "snd_pcm_type"}, {&a.descriptorCount, "snd_pcm_poll_descriptors_count"}, {&a.setParams, "snd_pcm_set_params"}, {&a.read, "snd_pcm_readi"}, {&a.write, "snd_pcm_writei"}, {&a.wait, "snd_pcm_wait"}, {&a.infoMalloc, "snd_pcm_info_malloc"}, {&a.infoFree, "snd_pcm_info_free"}, {&a.info, "snd_pcm_info"}, {&a.infoCard, "snd_pcm_info_get_card"}, {&a.infoDevice, "snd_pcm_info_get_device"}, {&a.infoSubdevice, "snd_pcm_info_get_subdevice"}, {&a.infoStream, "snd_pcm_info_get_stream"}, {&a.descriptors, "snd_pcm_poll_descriptors"},
	} {
		purego.RegisterLibFunc(symbol.fn, a.library, symbol.name)
	}
	for _, symbol := range []struct {
		fn   any
		name string
	}{
		{&a.swMalloc, "snd_pcm_sw_params_malloc"}, {&a.swFree, "snd_pcm_sw_params_free"},
		{&a.swCurrent, "snd_pcm_sw_params_current"}, {&a.swApply, "snd_pcm_sw_params"},
		{&a.swStartThreshold, "snd_pcm_sw_params_set_start_threshold"},
	} {
		purego.RegisterLibFunc(symbol.fn, a.library, symbol.name)
	}
	return a, nil
}
func nativeAvailable() bool {
	a, err := loadALSA([]string{"libasound.so.2", "libasound.so"})
	if err != nil {
		return false
	}
	_ = purego.Dlclose(a.library)
	return true
}

func (a *alsa) openConfiguration(config string, stream int32) (p alsaPCM, err error) {
	if code := a.loadConfig(&p.config, config, uintptr(len(config))); code < 0 {
		return p, fmt.Errorf("%w: ALSA local configuration (%d)", ErrUnavailable, code)
	}
	// Local numeric-only configuration does not read ALSA_CONFIG_PATH,
	// ~/.asoundrc, default, plug, dmix or another application's audio route.
	const nonblock = 1 | 0x10000 | 0x20000 | 0x40000 | 0x80000
	if code := a.openPCM(&p.handle, "vocat_bound", stream, nonblock, p.config); code < 0 {
		a.close(&p)
		if code == -16 {
			return p, ErrBusy
		}
		return p, fmt.Errorf("%w: ALSA open (%d)", ErrUnavailable, code)
	}
	return p, nil
}
func (a *alsa) close(p *alsaPCM) {
	if p.handle != 0 {
		_ = a.drop(p.handle)
		_ = a.closePCM(p.handle)
		p.handle = 0
	}
	if p.config != 0 {
		_ = a.deleteConfig(p.config)
		p.config = 0
	}
}
func (a *alsa) configure(p alsaPCM, capture bool) error {
	// ALSA ABI: S16_LE=2, RW_INTERLEAVED=3. No silent conversion or fallback.
	if code := a.setParams(p.handle, 2, 3, 1, 8000, 0, 100000); code < 0 {
		return fmt.Errorf("%w: ALSA parameters (%d)", ErrFormat, code)
	}
	// snd_pcm_set_params defaults start_threshold to the whole buffer. With
	// 20 ms nonblocking reads, capture never starts if that buffer is larger
	// than 160 frames. Keep preparation idle, then let the first actual I/O
	// start capture or playback after the controller has confirmed active.
	var params uintptr
	if code := a.swMalloc(&params); code < 0 || params == 0 {
		return fmt.Errorf("%w: ALSA software parameters allocation (%d)", ErrUnavailable, code)
	}
	defer a.swFree(params)
	if code := a.swCurrent(p.handle, params); code < 0 {
		return fmt.Errorf("%w: ALSA software parameters (%d)", ErrFormat, code)
	}
	threshold := uintptr(frameSamples)
	if capture {
		threshold = 1
	}
	if code := a.swStartThreshold(p.handle, params, threshold); code < 0 {
		return fmt.Errorf("%w: ALSA start threshold (%d)", ErrFormat, code)
	}
	if code := a.swApply(p.handle, params); code < 0 {
		return fmt.Errorf("%w: ALSA apply software parameters (%d)", ErrFormat, code)
	}
	return nil
}
func (a *alsa) verify(p alsaPCM, b *Binding, capture bool) error {
	if a.pcmType(p.handle) != 0 {
		return ErrChanged
	} // SND_PCM_TYPE_HW only.
	var info uintptr
	if a.infoMalloc(&info) < 0 || info == 0 {
		return ErrUnavailable
	}
	defer a.infoFree(info)
	stream := int32(0)
	device := b.pair.PlaybackDevice
	node := b.playback
	pin := b.pinned[5].info
	if capture {
		stream = 1
		device = b.pair.CaptureDevice
		node = b.capture
		pin = b.pinned[4].info
	}
	if a.info(p.handle, info) < 0 || a.infoCard(info) != int32(b.pair.Card) || a.infoDevice(info) != uint32(device) || a.infoSubdevice(info) != 0 || a.infoStream(info) != stream {
		return ErrChanged
	}
	if a.descriptorCount(p.handle) != 1 {
		return ErrChanged
	}
	var descriptor pollDescriptor
	if a.descriptors(p.handle, &descriptor, 1) != 1 || descriptor.FD < 0 {
		return ErrChanged
	}
	actual, err := os.Stat("/proc/self/fd/" + strconv.Itoa(int(descriptor.FD)))
	if err != nil || !os.SameFile(pin, actual) {
		return ErrChanged
	}
	now, err := os.Stat(node)
	if err != nil || !os.SameFile(pin, now) {
		return ErrChanged
	}
	return nil
}
func openNative(ctx context.Context, b *Binding) (pcm, error) {
	if err := b.Validate(ctx); err != nil {
		return nil, err
	}
	a, err := loadALSA([]string{"libasound.so.2", "libasound.so"})
	if err != nil {
		return nil, err
	}
	d := &alsaDuplex{api: a}
	ok := false
	defer func() {
		if !ok {
			_ = d.close()
		}
	}()
	for _, capture := range []bool{true, false} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := b.Validate(ctx); err != nil {
			return nil, err
		}
		stream := int32(0)
		device := b.pair.PlaybackDevice
		target := &d.playback
		if capture {
			stream = 1
			device = b.pair.CaptureDevice
			target = &d.capture
		}
		// hw is built into this loaded ALSA library. An explicit relative lib
		// name would instead be resolved under ALSA's plugin directory.
		config := fmt.Sprintf("pcm_type.hw { open \"_snd_pcm_hw_open\" }\npcm.vocat_bound { type hw card %d device %d subdevice 0 }\n", b.pair.Card, device)
		*target, err = a.openConfiguration(config, stream)
		if err != nil {
			return nil, err
		}
		if err := a.verify(*target, b, capture); err != nil {
			return nil, err
		}
		if err := b.Validate(ctx); err != nil {
			return nil, err
		}
		if err := a.configure(*target, capture); err != nil {
			return nil, err
		}
	}
	if err := b.Validate(ctx); err != nil {
		return nil, err
	}
	ok = true
	return d, nil
}
func (d *alsaDuplex) transfer(p alsaPCM, samples []int16, capture bool) (int, error) {
	if len(samples) == 0 {
		return 0, ErrFormat
	}
	var n int
	if capture {
		n = d.api.read(p.handle, &samples[0], uintptr(len(samples)))
	} else {
		n = d.api.write(p.handle, &samples[0], uintptr(len(samples)))
	}
	runtime.KeepAlive(samples)
	if n >= 0 {
		if n > 0 {
			if capture {
				d.readFaults = 0
			} else {
				d.writeFaults = 0
			}
		}
		return n, nil
	}
	switch n {
	case -11, -4:
		return 0, errAgain
	case -32:
		if d.recoverXRun(p, capture) {
			return 0, errAgain
		}
	}
	// Suspend/removal is an owner failure, not permission to reopen defaults,
	// power-cycle a card or block in an unbounded ALSA resume/recover loop.
	return 0, fmt.Errorf("%w: ALSA transfer (%d)", ErrUnavailable, n)
}
func (d *alsaDuplex) read(samples []int16) (int, error) { return d.transfer(d.capture, samples, true) }
func (d *alsaDuplex) write(samples []int16) (int, error) {
	return d.transfer(d.playback, samples, false)
}
func (d *alsaDuplex) wait(ctx context.Context, capture bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := d.playback
	if capture {
		p = d.capture
	}
	code := d.api.wait(p.handle, 10)
	if err := ctx.Err(); err != nil {
		return err
	}
	if code >= 0 || code == -4 {
		return nil
	}
	if code == -32 && d.recoverXRun(p, capture) {
		return nil
	}
	return fmt.Errorf("%w: ALSA wait (%d)", ErrUnavailable, code)
}
func (d *alsaDuplex) recoverXRun(p alsaPCM, capture bool) bool {
	faults := &d.writeFaults
	if capture {
		faults = &d.readFaults
	}
	*faults++
	return *faults <= 3 && d.api.prepare(p.handle) == 0
}
func (d *alsaDuplex) close() error {
	if d.api == nil {
		return nil
	}
	d.api.close(&d.capture)
	d.api.close(&d.playback)
	err := purego.Dlclose(d.api.library)
	d.api = nil
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	return nil
}
