//go:build linux && (amd64 || arm64 || 386 || arm) && !vocat_static

package ims

import (
	"errors"
	"github.com/ebitengine/purego"
	"runtime"
	"sync"
)

// The library owns no Go pointers after a call. Each call instance owns
// separate encoder/decoder states and keeps them alive under a single lock.
type nativeAMR struct {
	mu                        sync.Mutex
	library, encoder, decoder uintptr
	encode                    func(uintptr, int32, *int16, *byte, int32) int32
	decode                    func(uintptr, *byte, *int16, int32)
	exitEncoder, exitDecoder  func(uintptr)
}

func openNativeAMR() (codec amrCodec, err error) {
	var handle uintptr
	for _, name := range []string{"libopencore-amrnb.so.0", "libopencore-amrnb.so"} {
		handle, err = purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, errAMRUnavailable
	}
	n := &nativeAMR{library: handle}
	defer func() {
		if recovered := recover(); recovered != nil {
			n.Close()
			codec = nil
			err = errAMRUnavailable
		}
	}()
	var initEncoder func(int32) uintptr
	var initDecoder func() uintptr
	purego.RegisterLibFunc(&initEncoder, handle, "Encoder_Interface_init")
	purego.RegisterLibFunc(&initDecoder, handle, "Decoder_Interface_init")
	purego.RegisterLibFunc(&n.exitEncoder, handle, "Encoder_Interface_exit")
	purego.RegisterLibFunc(&n.exitDecoder, handle, "Decoder_Interface_exit")
	purego.RegisterLibFunc(&n.encode, handle, "Encoder_Interface_Encode")
	purego.RegisterLibFunc(&n.decode, handle, "Decoder_Interface_Decode")
	n.encoder = initEncoder(0)
	n.decoder = initDecoder()
	if n.encoder == 0 || n.decoder == 0 {
		n.Close()
		return nil, errAMRUnavailable
	}
	return n, nil
}

func (n *nativeAMR) Encode(pcm []int16, mode int) ([]byte, error) {
	if len(pcm) != 160 || mode < 0 || mode > 7 {
		return nil, errors.New("ims: AMR encode requires 160 samples and mode 0..7")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.encoder == 0 {
		return nil, errAMRUnavailable
	}
	frame := make([]byte, 32)
	// OpenCORE's const-facing wrapper casts speech to a mutable buffer.
	// Protect caller PCM from its in-place preprocessing between frames.
	scratch := append([]int16(nil), pcm...)
	count := n.encode(n.encoder, int32(mode), &scratch[0], &frame[0], 1)
	runtime.KeepAlive(scratch)
	if count < 1 || count > 32 {
		return nil, errors.New("ims: invalid native AMR encoder result")
	}
	frame = frame[:count]
	if _, err := amrFrameBits(frame); err != nil || int(frame[0]>>3&15) != mode {
		return nil, errors.New("ims: native AMR encoder returned wrong mode")
	}
	return frame, nil
}

func (n *nativeAMR) Decode(frame []byte) ([]int16, error) {
	if _, err := amrFrameBits(frame); err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.decoder == 0 {
		return nil, errAMRUnavailable
	}
	pcm := make([]int16, 160)
	bad := int32(0)
	if frame[0]&4 == 0 {
		bad = 1
	}
	scratch := append([]byte(nil), frame...)
	n.decode(n.decoder, &scratch[0], &pcm[0], bad)
	runtime.KeepAlive(scratch)
	return pcm, nil
}

func (n *nativeAMR) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.encoder != 0 {
		n.exitEncoder(n.encoder)
		n.encoder = 0
	}
	if n.decoder != 0 {
		n.exitDecoder(n.decoder)
		n.decoder = 0
	}
	if n.library != 0 {
		_ = purego.Dlclose(n.library)
		n.library = 0
	}
}
