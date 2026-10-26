// Package callrecord saves call-bound PCM without adding a media consumer.
// Media threads enqueue bounded copies; only recording workers touch disk.
package callrecord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const SampleRate = 8000
const MaxActive = 64
const SettingKey = "calls.recordings"
const maxEntries = 10000

var validID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var ErrUnavailable = errors.New("call recordings unavailable")
var ErrActive = errors.New("the recording is still active")

type Config struct {
	Enabled       bool `json:"enabled"`
	KeepCalls     int  `json:"keep_calls"`
	MaxMinutes    int  `json:"max_minutes"`
	MaxStorageMiB int  `json:"max_storage_mib"`
}

func DefaultConfig() Config {
	return Config{Enabled: true, KeepCalls: 20, MaxMinutes: 180, MaxStorageMiB: 1024}
}
func (c Config) Validate() error {
	if c.KeepCalls < 1 || c.KeepCalls > 1000 {
		return errors.New("keep_calls must be 1–1000")
	}
	if c.MaxMinutes < 1 || c.MaxMinutes > 1440 {
		return errors.New("max_minutes must be 1–1440")
	}
	if c.MaxStorageMiB < 16 || c.MaxStorageMiB > 16384 {
		return errors.New("max_storage_mib must be 16–16384")
	}
	return nil
}

type Call struct {
	DeviceID      string    `json:"device_id"`
	ICCID         string    `json:"iccid"`
	CallID        string    `json:"call_id"`
	Number        string    `json:"number"`
	Direction     string    `json:"direction"`
	Access        string    `json:"access"`
	CallStartedAt time.Time `json:"call_started_at"`
}
type Metadata struct {
	Call
	ID              string     `json:"id"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	DurationMS      int64      `json:"duration_ms"`
	Bytes           int64      `json:"bytes"`
	State           string     `json:"state"`
	StopReason      string     `json:"stop_reason,omitempty"`
	DroppedSamples  uint64     `json:"dropped_samples"`
	UplinkSamples   uint64     `json:"uplink_samples"`
	DownlinkSamples uint64     `json:"downlink_samples"`
}
type Status struct {
	Config    Config `json:"config"`
	Directory string `json:"directory"`
	UsedBytes int64  `json:"used_bytes"`
	Active    int    `json:"active"`
	LastError string `json:"last_error,omitempty"`
}
type Factory func(Call) *Sink
type Service struct {
	root       *os.Root
	directory  string
	logger     *slog.Logger
	mu         sync.Mutex
	config     Config
	closed     bool
	rootClosed bool // filesMu
	lastError  string
	active     map[string]*Sink
	records    map[string]Metadata
	filesMu    sync.Mutex
	workers    sync.WaitGroup
	used       atomic.Int64
	limit      atomic.Int64
}

func New(directory string, c Config, logger *slog.Logger) (*Service, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	s := &Service{root: root, directory: abs, config: c, logger: logger, active: make(map[string]*Sink), records: make(map[string]Metadata)}
	s.limit.Store(int64(c.MaxStorageMiB) << 20)
	if err = s.recover(); err != nil {
		root.Close()
		return nil, err
	}
	if err = s.Prune(); err != nil {
		root.Close()
		return nil, err
	}
	return s, nil
}
func (s *Service) report(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.lastError = err.Error()
	s.mu.Unlock()
	if s.logger != nil {
		s.logger.Warn("call recording stopped", "category", "call", "error", err)
	}
}
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Config: s.config, Directory: s.directory, UsedBytes: s.used.Load(), Active: len(s.active), LastError: s.lastError}
}
func (s *Service) Configure(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrUnavailable
	}
	s.config = c
	s.lastError = ""
	s.limit.Store(int64(c.MaxStorageMiB) << 20)
	var stop []*Sink
	if !c.Enabled {
		for _, sink := range s.active {
			stop = append(stop, sink)
		}
	}
	s.mu.Unlock()
	for _, sink := range stop {
		sink.Close("disabled")
	}
	err := s.Prune()
	if err != nil {
		s.report(err)
	}
	return err
}

// Start performs no filesystem work. Returning nil must never prevent calling.
func (s *Service) Start(info Call) *Sink {
	if s == nil || info.DeviceID == "" || info.CallID == "" || info.ICCID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.config.Enabled {
		return nil
	}
	if len(info.DeviceID) > 256 || len(info.CallID) > 256 || len(info.ICCID) > 22 || len(info.Number) > 512 || len(info.Access) > 32 || len(info.Direction) > 16 {
		s.lastError = "recording identity exceeds supported limits"
		return nil
	}
	if len(s.active) >= MaxActive {
		s.lastError = "too many active call recordings"
		return nil
	}
	for _, running := range s.active {
		if running.meta.DeviceID == info.DeviceID && running.meta.CallID == info.CallID {
			return nil
		}
	}
	var seed [16]byte
	if _, err := rand.Read(seed[:]); err != nil {
		s.lastError = err.Error()
		return nil
	}
	now := time.Now()
	metadata := Metadata{Call: info, ID: hex.EncodeToString(seed[:]), StartedAt: now.UTC(), State: "recording"}
	sink := &Sink{service: s, meta: metadata, started: now, config: s.config, frames: make(chan frame, 128)}
	s.active[metadata.ID] = sink
	s.workers.Add(1)
	go sink.run()
	return sink
}

func (s *Service) List(device string) []Metadata {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Metadata, 0, len(s.records)+len(s.active))
	for _, m := range s.records {
		if device == "" || m.DeviceID == device {
			result = append(result, m)
		}
	}
	for _, sink := range s.active {
		if device == "" || sink.meta.DeviceID == device {
			result = append(result, sink.meta)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StartedAt.After(result[j].StartedAt) })
	return result
}

func (s *Service) Open(id string) (*os.File, Metadata, error) {
	if !validID.MatchString(id) {
		return nil, Metadata{}, os.ErrNotExist
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	s.mu.Lock()
	m, exists := s.records[id]
	_, active := s.active[id]
	closed := s.closed
	s.mu.Unlock()
	if active {
		return nil, m, ErrActive
	}
	if closed || !exists {
		return nil, m, os.ErrNotExist
	}
	f, err := s.root.Open(id + ".wav")
	if err != nil {
		return nil, m, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != m.Bytes {
		f.Close()
		return nil, m, ErrUnavailable
	}
	return f, m, nil
}
func (s *Service) Delete(id string) error {
	if !validID.MatchString(id) {
		return os.ErrNotExist
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	s.mu.Lock()
	_, active := s.active[id]
	_, exists := s.records[id]
	closed := s.closed
	s.mu.Unlock()
	if active {
		return ErrActive
	}
	if closed {
		return ErrUnavailable
	}
	if !exists {
		return os.ErrNotExist
	}
	return s.remove(id)
}

// filesMu protects publication, opening and deletion. Never delete an active file.
func (s *Service) remove(id string) error {
	info, err := s.root.Stat(id + ".wav")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err = s.root.Remove(id + ".wav"); err != nil {
			return err
		}
		s.used.Add(-info.Size())
	}
	if err = s.root.Remove(id + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.mu.Lock()
	delete(s.records, id)
	s.mu.Unlock()
	return nil
}
func (s *Service) oldest() []Metadata {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]Metadata, 0, len(s.records))
	for _, m := range s.records {
		items = append(items, m)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].StartedAt.Before(items[j].StartedAt) })
	return items
}
func (s *Service) Prune() error {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	s.mu.Lock()
	keep := s.config.KeepCalls
	s.mu.Unlock()
	items := s.oldest()
	for i, m := range items {
		if len(items)-i <= keep && s.used.Load() <= s.limit.Load() {
			break
		}
		if err := s.remove(m.ID); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) allocate(n int64) bool {
	for {
		old := s.used.Load()
		if old+n > s.limit.Load() {
			break
		}
		if s.used.CompareAndSwap(old, old+n) {
			return true
		}
	}
	// This runs only on a recording worker, never on a media thread.
	s.filesMu.Lock()
	for _, m := range s.oldest() {
		if s.used.Load()+n <= s.limit.Load() {
			break
		}
		if err := s.remove(m.ID); err != nil {
			s.report(err)
			break
		}
	}
	s.filesMu.Unlock()
	for {
		old := s.used.Load()
		if old+n > s.limit.Load() {
			return false
		}
		if s.used.CompareAndSwap(old, old+n) {
			return true
		}
	}
}
func (s *Service) publish(m Metadata) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	name := m.ID + ".json.tmp"
	f, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = s.root.Rename(name, m.ID+".json")
	}
	if err != nil {
		_ = s.root.Remove(name)
	}
	return err
}
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	active := make([]*Sink, 0, len(s.active))
	for _, sink := range s.active {
		active = append(active, sink)
	}
	s.mu.Unlock()
	for _, sink := range active {
		sink.Close("shutdown")
	}
	done := make(chan struct{})
	go func() { s.workers.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if s.rootClosed {
		return nil
	}
	s.rootClosed = true
	return s.root.Close()
}

func (s *Service) recover() error {
	dir, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > maxEntries {
		return errors.New("recording directory contains too many entries")
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".wav") || !validID.MatchString(strings.TrimSuffix(name, ".wav")) {
			continue
		}
		id := strings.TrimSuffix(name, ".wav")
		if !entry.Type().IsRegular() {
			return errors.New("recording file is not a regular file")
		}
		f, err := s.root.OpenFile(name, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		s.used.Add(info.Size())
		m := Metadata{ID: id, StartedAt: info.ModTime().UTC(), State: "interrupted", StopReason: "process_interrupted"}
		jf, jsonErr := s.root.Open(id + ".json")
		if jsonErr == nil {
			decoder := json.NewDecoder(io.LimitReader(jf, 16385))
			jsonErr = decoder.Decode(&m)
			jf.Close()
		}
		if jsonErr != nil && !errors.Is(jsonErr, os.ErrNotExist) {
			f.Close()
			return errors.New("invalid recording metadata")
		}
		if m.ID != id {
			f.Close()
			return errors.New("recording metadata identity mismatch")
		}
		header := make([]byte, 44)
		_, err = io.ReadFull(f, header)
		if info.Size() < 44 {
			// A process can exit between O_EXCL creation and its first header
			// write. Preserve this as an empty interrupted recording.
			if _, err = f.WriteAt(wavHeader(0), 0); err != nil {
				f.Close()
				return err
			}
			s.used.Add(44 - info.Size())
			info, err = f.Stat()
			if err != nil {
				f.Close()
				return err
			}
			header = wavHeader(0)
		}
		if err != nil || !validWAVHeader(header) || info.Size() < 44 || info.Size() > int64(SampleRate)*4*86400+44 {
			f.Close()
			return errors.New("invalid stored recording WAV")
		}
		bytes := info.Size() - 44
		aligned := bytes - bytes%4
		if jsonErr != nil || m.State == "recording" || bytes != aligned || !wavSizesMatch(header, aligned) || m.Bytes != info.Size() {
			if err = f.Truncate(44 + aligned); err == nil {
				_, err = f.WriteAt(wavHeader(aligned), 0)
			}
			if err == nil {
				err = f.Sync()
			}
			if err != nil {
				f.Close()
				return err
			}
			s.used.Add(aligned - bytes)
			ended := info.ModTime().UTC()
			m.EndedAt = &ended
			m.State = "interrupted"
			m.StopReason = "process_interrupted"
			m.Bytes = aligned + 44
			m.DurationMS = aligned * 1000 / (SampleRate * 4)
			// A temp sidecar can remain if the process died during publication.
			_ = s.root.Remove(id + ".json.tmp")
			if err = s.publish(m); err != nil {
				f.Close()
				return err
			}
		}
		f.Close()
		s.records[id] = m
	}
	return nil
}
