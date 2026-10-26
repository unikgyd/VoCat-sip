package callrecord

import (
	"encoding/binary"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const samplesPerFrame = 160
const stereoFrameBytes = samplesPerFrame * 4

type Direction uint8

const (
	Uplink Direction = iota
	Downlink
)

type frame struct {
	at        time.Time
	direction Direction
	samples   []int16
}
type Sink struct {
	service *Service
	meta    Metadata // immutable after Start; worker publishes a copy
	started time.Time
	config  Config
	mu      sync.Mutex
	closed  bool
	ended   time.Time
	reason  string
	frames  chan frame
	dropped atomic.Uint64
}

// PCM copies at most 400 ms. A slow disk cannot stall RTP or ALSA.
func (s *Sink) PCM(direction Direction, samples []int16) {
	if s == nil || len(samples) == 0 {
		return
	}
	if direction > Downlink || len(samples) > 3200 {
		s.dropped.Add(uint64(len(samples)))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.frames <- frame{at: time.Now(), direction: direction, samples: append([]int16(nil), samples...)}:
	default:
		s.dropped.Add(uint64(len(samples)))
	}
}

// Close never waits for a filesystem operation. It is safe under call locks.
func (s *Sink) Close(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.reason = reason
	s.ended = time.Now()
	close(s.frames)
}

func (s *Sink) run() {
	defer s.service.workers.Done()
	metadata := s.meta
	var failure error
	defer func() {
		s.Close("recording_failed")
		s.service.filesMu.Lock()
		if failure != nil && metadata.State == "recording" {
			metadata.State = "incomplete"
			metadata.StopReason = "write_failed"
		}
		if metadata.EndedAt == nil {
			ended := time.Now().UTC()
			metadata.EndedAt = &ended
		}
		s.service.mu.Lock()
		delete(s.service.active, metadata.ID)
		s.service.mu.Unlock()
		if metadata.Bytes >= 44 {
			if err := s.service.publish(metadata); err != nil {
				failure = errors.Join(failure, err)
				metadata.State = "incomplete"
				metadata.StopReason = "metadata_write_failed"
			}
			s.service.mu.Lock()
			s.service.records[metadata.ID] = metadata
			s.service.mu.Unlock()
		}
		s.service.filesMu.Unlock()
		if failure != nil {
			s.service.report(failure)
		}
		if err := s.service.Prune(); err != nil {
			s.service.report(err)
		}
	}()
	if !s.service.allocate(44) {
		failure = errors.New("recording storage limit reached")
		return
	}
	f, err := s.service.root.OpenFile(metadata.ID+".wav", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		s.service.used.Add(-44)
		failure = err
		return
	}
	defer f.Close()
	n, err := f.Write(wavHeader(0))
	s.service.used.Add(int64(n) - 44)
	if err != nil || n != 44 {
		failure = errors.Join(err, errors.New("could not create recording WAV header"))
		f.Close()
		if removeErr := s.service.root.Remove(metadata.ID + ".wav"); removeErr == nil {
			s.service.used.Add(-int64(n))
		}
		return
	}
	metadata.Bytes = 44
	// Publish the initial identity before accepting disk data. On restart a
	// recording state is repaired and explicitly marked interrupted.
	s.service.filesMu.Lock()
	err = s.service.publish(metadata)
	s.service.filesMu.Unlock()
	if err != nil {
		failure = err
		return
	}
	blocks := make(map[int64]*[samplesPerFrame * 2]int16)
	var cursors [2]int64
	var flushed int64
	maxFrames := int64(s.config.MaxMinutes) * 60 * 50
	stopReason := ""
	flush := func(target int64) error {
		target = min(maxFrames, max(flushed, target))
		for flushed < target {
			count := min(int64(50), target-flushed)
			data := make([]byte, int(count)*stereoFrameBytes)
			for block := int64(0); block < count; block++ {
				if pcm := blocks[flushed+block]; pcm != nil {
					for i, sample := range pcm {
						binary.LittleEndian.PutUint16(data[int(block)*stereoFrameBytes+i*2:], uint16(sample))
					}
				}
				delete(blocks, flushed+block)
			}
			if !s.service.allocate(int64(len(data))) {
				stopReason = "storage_limit"
				return errors.New("recording storage limit reached")
			}
			n, err := f.Write(data)
			s.service.used.Add(int64(n) - int64(len(data)))
			metadata.Bytes += int64(n)
			if err != nil || n != len(data) {
				return errors.Join(err, errors.New("recording PCM write was incomplete"))
			}
			flushed += count
		}
		return nil
	}
	accept := func(value frame) {
		// Align each direction on its own sample cursor. Small arrival jitter
		// must not overlap neighboring packets; long gaps become WAV silence.
		position := max(int64(0), int64(value.at.Sub(s.started))*SampleRate/int64(time.Second)-int64(len(value.samples)))
		position = position / samplesPerFrame * samplesPerFrame
		cursor := cursors[value.direction]
		if position < cursor {
			if cursor-position <= SampleRate/5 {
				position = cursor
			} else {
				s.dropped.Add(uint64(len(value.samples)))
				return
			}
		}
		if position < int64(flushed)*samplesPerFrame || position > int64(maxFrames)*samplesPerFrame || position-cursor > SampleRate*86400 {
			s.dropped.Add(uint64(len(value.samples)))
			return
		}
		// Never let a malformed/bursty producer fill duration-sized memory.
		if position/samplesPerFrame-flushed > 200 {
			s.dropped.Add(uint64(len(value.samples)))
			return
		}
		for i, sample := range value.samples {
			samplePosition := position + int64(i)
			block := samplePosition / samplesPerFrame
			if block >= maxFrames {
				break
			}
			pcm := blocks[block]
			if pcm == nil {
				pcm = &[samplesPerFrame * 2]int16{}
				blocks[block] = pcm
			}
			pcm[(samplePosition%samplesPerFrame)*2+int64(value.direction)] = sample
		}
		cursors[value.direction] = max(cursor, position+int64(len(value.samples)))
		if value.direction == Uplink {
			metadata.UplinkSamples += uint64(len(value.samples))
		} else {
			metadata.DownlinkSamples += uint64(len(value.samples))
		}
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
loop:
	for {
		select {
		case value, ok := <-s.frames:
			if !ok {
				break loop
			}
			accept(value)
		case now := <-ticker.C:
			if now.Sub(s.started) >= time.Duration(s.config.MaxMinutes)*time.Minute {
				stopReason = "duration_limit"
				s.Close(stopReason)
			}
			// Hold a short merge window for the other direction and scheduling.
			target := max(int64(0), int64(now.Sub(s.started)-250*time.Millisecond)/int64(20*time.Millisecond))
			if err = flush(target); err != nil {
				failure = err
				break loop
			}
		}
	}
	s.Close("call_ended")
	s.mu.Lock()
	ended, reason := s.ended, s.reason
	s.mu.Unlock()
	if stopReason != "" {
		reason = stopReason
	}
	if failure == nil {
		endFrames := (int64(ended.Sub(s.started)) + int64(20*time.Millisecond) - 1) / int64(20*time.Millisecond)
		endFrames = max(endFrames, (max(cursors[0], cursors[1])+samplesPerFrame-1)/samplesPerFrame)
		if err = flush(endFrames); err != nil {
			failure = err
			if stopReason != "" {
				reason = stopReason
			}
		}
	}
	// A failed write can leave a partial stereo frame. Expose only complete
	// samples with a valid RIFF size, while retaining an incomplete status.
	aligned := (metadata.Bytes - 44) / 4 * 4
	if err = f.Truncate(44 + aligned); err == nil {
		s.service.used.Add(44 + aligned - metadata.Bytes)
		metadata.Bytes = 44 + aligned
		_, err = f.WriteAt(wavHeader(aligned), 0)
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		failure = errors.Join(failure, err)
	}
	if err = f.Close(); err != nil {
		failure = errors.Join(failure, err)
	}
	metadata.EndedAt = &ended
	metadata.DurationMS = aligned * 1000 / (SampleRate * 4)
	metadata.DroppedSamples = s.dropped.Load()
	metadata.StopReason = reason
	metadata.State = "complete"
	if metadata.DroppedSamples > 0 || failure != nil || reason != "call_ended" {
		metadata.State = "incomplete"
	}
	if metadata.UplinkSamples+metadata.DownlinkSamples == 0 {
		metadata.State = "incomplete"
		metadata.StopReason = "no_audio"
	}
	if reason == "storage_limit" || reason == "duration_limit" {
		metadata.State = "truncated"
	}
}

func wavHeader(dataBytes int64) []byte {
	b := make([]byte, 44)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(dataBytes+36))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 2)
	binary.LittleEndian.PutUint32(b[24:], SampleRate)
	binary.LittleEndian.PutUint32(b[28:], SampleRate*4)
	binary.LittleEndian.PutUint16(b[32:], 4)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(dataBytes))
	return b
}
func wavDataSize(b []byte) int64 {
	if len(b) != 44 {
		return -1
	}
	return int64(binary.LittleEndian.Uint32(b[40:44]))
}
func validWAVHeader(b []byte) bool {
	if len(b) != 44 {
		return false
	}
	expected := wavHeader(wavDataSize(b))
	for i := range b {
		if i >= 4 && i < 8 || i >= 40 {
			continue
		}
		if b[i] != expected[i] {
			return false
		}
	}
	return true
}
func wavSizesMatch(b []byte, size int64) bool {
	return wavDataSize(b) == size && int64(binary.LittleEndian.Uint32(b[4:8])) == size+36
}
