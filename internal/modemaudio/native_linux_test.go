//go:build linux && (amd64 || arm64 || 386 || arm) && !vocat_static

package modemaudio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ebitengine/purego"
)

func TestUSBAudioNativeLocalNullPCMAndSignedLongABIWithoutHardware(t *testing.T) {
	a, err := loadALSA([]string{"libasound.so.2", "libasound.so"})
	if err != nil {
		if os.Getenv("VOCAT_REQUIRE_USB_AUDIO_TEST") == "1" {
			t.Fatal("ALSA required in this isolated acceptance run", err)
		}
		t.Skip("optional system ALSA library unavailable")
	}
	defer purego.Dlclose(a.library)
	// A global configuration would fail if consulted. The only opened PCM is
	// the in-memory NULL plugin explicitly defined in this private tree.
	t.Setenv("ALSA_CONFIG_PATH", filepath.Join(t.TempDir(), "not-a-global-config"))
	f := newAudioFixture(t)
	_, binding := f.one(t)
	for _, stream := range []int32{0, 1} {
		config := "pcm_type.null { open \"_snd_pcm_null_open\" }\npcm.vocat_bound { type null }\n"
		p, err := a.openConfiguration(config, stream)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer a.close(&p)
			if a.pcmType(p.handle) == 0 {
				t.Fatal("NULL was reported as hardware")
			}
			if err := a.verify(p, binding, stream == 1); !errors.Is(err, ErrChanged) {
				t.Fatal("non-hardware accepted", err)
			}
			if err := a.configure(p, stream == 1); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 8; i++ {
				samples := make([]int16, 160)
				var n int
				if stream == 1 {
					n = a.read(p.handle, &samples[0], 160)
				} else {
					for j := range samples {
						samples[j] = int16(j*17 - 1000)
					}
					n = a.write(p.handle, &samples[0], 160)
				}
				if n != 160 {
					t.Fatal("NULL transfer", n)
				}
			}
			_ = a.drop(p.handle)
			samples := make([]int16, 160)
			if n := a.read(p.handle, &samples[0], 160); n >= 0 {
				t.Fatal("negative C long lost its sign", n)
			}
		}()
	}
}
func TestUSBAudioNativeMissingLibraryDoesNotPretendCapability(t *testing.T) {
	if _, e := loadALSA([]string{filepath.Join(t.TempDir(), "absent-libasound.so.2")}); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
}

func TestUSBAudioNativeTwentyMillisecondIOStartsPreparedStreams(t *testing.T) {
	a, err := loadALSA([]string{"libasound.so.2", "libasound.so"})
	if err != nil {
		if os.Getenv("VOCAT_REQUIRE_USB_AUDIO_TEST") == "1" {
			t.Fatal(err)
		}
		t.Skip("optional system ALSA library unavailable")
	}
	defer purego.Dlclose(a.library)
	var state func(uintptr) int32
	var getThreshold func(uintptr, *uintptr) int32
	purego.RegisterLibFunc(&state, a.library, "snd_pcm_state")
	purego.RegisterLibFunc(&getThreshold, a.library, "snd_pcm_sw_params_get_start_threshold")
	for _, stream := range []int32{0, 1} {
		p, err := a.openConfiguration("pcm_type.null { open \"_snd_pcm_null_open\" }\npcm.vocat_bound { type null }\n", stream)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer a.close(&p)
			if err := a.configure(p, stream == 1); err != nil {
				t.Fatal(err)
			}
			if state(p.handle) != 2 { // PREPARED, no capture/playback during admission.
				t.Fatal("configuration started physical I/O before the call was active")
			}
			var params uintptr
			if a.swMalloc(&params) < 0 || params == 0 {
				t.Fatal("software parameter allocation")
			}
			defer a.swFree(params)
			var threshold uintptr
			if a.swCurrent(p.handle, params) < 0 || getThreshold(params, &threshold) < 0 || threshold == 0 || threshold > 160 {
				t.Fatal("20 ms I/O cannot meet ALSA's start threshold", threshold)
			}
			samples := make([]int16, 160)
			var n int
			if stream == 1 {
				n = a.read(p.handle, &samples[0], 160)
			} else {
				n = a.write(p.handle, &samples[0], 160)
			}
			if n != 160 || state(p.handle) != 3 { // RUNNING only after actual I/O.
				t.Fatal("first 20 ms frame failed to start the stream", n, state(p.handle))
			}
		}()
	}
}
func TestUSBAudioNativeMetadataAndFileDescriptorAreBothRequired(t *testing.T) {
	for _, kind := range []string{"valid", "type", "card", "device", "subdevice", "stream", "count", "fd"} {
		t.Run(kind, func(t *testing.T) {
			f := newAudioFixture(t)
			_, b := f.one(t)
			file, err := os.Open(b.capture)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			a := &alsa{pcmType: func(uintptr) int32 { return 0 }, infoMalloc: func(p *uintptr) int32 { *p = 1; return 0 }, infoFree: func(uintptr) {}, info: func(uintptr, uintptr) int32 { return 0 }, infoCard: func(uintptr) int32 { return int32(b.pair.Card) }, infoDevice: func(uintptr) uint32 { return uint32(b.pair.CaptureDevice) }, infoSubdevice: func(uintptr) uint32 { return 0 }, infoStream: func(uintptr) int32 { return 1 }, descriptorCount: func(uintptr) int32 { return 1 }, descriptors: func(_ uintptr, d *pollDescriptor, _ uint32) int32 { d.FD = int32(file.Fd()); return 1 }}
			switch kind {
			case "type":
				a.pcmType = func(uintptr) int32 { return 4 }
			case "card":
				a.infoCard = func(uintptr) int32 { return 0 }
			case "device":
				a.infoDevice = func(uintptr) uint32 { return 9 }
			case "subdevice":
				a.infoSubdevice = func(uintptr) uint32 { return 1 }
			case "stream":
				a.infoStream = func(uintptr) int32 { return 0 }
			case "count":
				a.descriptorCount = func(uintptr) int32 { return 2 }
			case "fd":
				a.descriptors = func(_ uintptr, d *pollDescriptor, _ uint32) int32 { d.FD = -1; return 1 }
			}
			err = a.verify(alsaPCM{handle: 1}, b, true)
			if (err == nil) != (kind == "valid") {
				t.Fatal(kind, err)
			}
		})
	}
}
func TestUSBAudioNativeXRunRecoveryIsBoundedAndSuspendFails(t *testing.T) {
	prepares := 0
	a := &alsa{prepare: func(uintptr) int32 { prepares++; return 0 }, read: func(uintptr, *int16, uintptr) int { return -32 }}
	d := &alsaDuplex{api: a, capture: alsaPCM{handle: 1}}
	for i := 0; i < 3; i++ {
		if _, e := d.read(make([]int16, 160)); !errors.Is(e, errAgain) {
			t.Fatal(e)
		}
	}
	if _, e := d.read(make([]int16, 160)); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	if prepares != 3 {
		t.Fatal(prepares)
	}
	a.read = func(uintptr, *int16, uintptr) int { return -86 }
	if _, e := d.read(make([]int16, 160)); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := d.wait(ctx, true); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
