package callrecord

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"
)

func TestCallRecordingKeepsStereoIdentityAndPrivateDownload(t *testing.T) {
	directory := t.TempDir()
	s, err := New(directory, DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	info := Call{DeviceID: "line-a", ICCID: "8901000000000000000", CallID: "original-call", Direction: "incoming", Access: "host_ims"}
	sink := s.Start(info)
	if sink == nil {
		t.Fatal("recording did not start")
	}
	if s.Start(info) != nil {
		t.Fatal("duplicate consumer created")
	}
	if err := s.Delete(sink.meta.ID); !errors.Is(err, ErrActive) {
		t.Fatal("active recording deleted", err)
	}
	up, down := make([]int16, 160), make([]int16, 160)
	for i := range up {
		up[i] = 1234
		down[i] = -2345
	}
	sink.PCM(Uplink, up)
	sink.PCM(Downlink, down)
	up[0] = 999 // Sink owns its copy.
	sink.Close("call_ended")
	s.workers.Wait()
	rows := s.List("line-a")
	if len(rows) != 1 || rows[0].Call != info || rows[0].State != "complete" || rows[0].UplinkSamples != 160 || rows[0].DownlinkSamples != 160 {
		t.Fatal(rows)
	}
	f, m, err := s.Open(rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 684 || string(data[:4]) != "RIFF" || binary.LittleEndian.Uint16(data[22:24]) != 2 || binary.LittleEndian.Uint32(data[40:44]) != uint32(len(data)-44) || m.Bytes != int64(len(data)) {
		t.Fatal("invalid stereo WAV")
	}
	if int16(binary.LittleEndian.Uint16(data[44:46])) != 1234 || int16(binary.LittleEndian.Uint16(data[46:48])) != -2345 {
		t.Fatal("stereo directions mixed or producer buffer retained")
	}
	if _, _, err := s.Open("../../outside"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unvalidated recording path", err)
	}
	if len(s.List("other-line")) != 0 {
		t.Fatal("device filter leaked another line")
	}
	if err := s.Delete(rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if len(s.List("")) != 0 || s.Status().UsedBytes != 0 {
		t.Fatal("deleted recording still accounted")
	}
}

func TestRecordingRetentionAndDisabledCapture(t *testing.T) {
	config := DefaultConfig()
	config.KeepCalls = 1
	s, err := New(t.TempDir(), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	for _, id := range []string{"first", "second"} {
		sink := s.Start(Call{DeviceID: "a", ICCID: "8901000000000000000", CallID: id})
		if sink == nil {
			t.Fatal("missing recording")
		}
		sink.PCM(Uplink, make([]int16, 160))
		sink.Close("call_ended")
		s.workers.Wait()
	}
	rows := s.List("")
	if len(rows) != 1 || rows[0].CallID != "second" {
		t.Fatal("retention removed wrong recording", rows)
	}
	config.Enabled = false
	if err := s.Configure(config); err != nil {
		t.Fatal(err)
	}
	if s.Start(Call{DeviceID: "a", ICCID: "8901000000000000000", CallID: "disabled"}) != nil {
		t.Fatal("disabled recorder admitted capture")
	}
}
