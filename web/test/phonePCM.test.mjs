import assert from "node:assert/strict";
import test from "node:test";
import { PhonePCM } from "../public/phone-pcm.js";

for (const rate of [44100, 48000]) {
  test(`microphone ${rate} Hz emits exact 20 ms, LE 8 kHz PCM`, () => {
    const pcm = new PhonePCM(rate), frames = [];
    const input = new Float32Array(rate).fill(0.5);
    for (let i = 0; i < rate; i += 128) pcm.capture(input.subarray(i, i + 128), (buffer, level) => frames.push({ buffer, level }));
    assert.equal(frames.length, 50);
    for (const { buffer, level } of frames) {
      assert.equal(buffer.byteLength, 320); assert.ok(Math.abs(level - 0.5) < 0.001);
      const view = new DataView(buffer);
      for (let i = 0; i < 160; i++) assert.equal(view.getInt16(i * 2, true), 16384);
    }
    pcm.muted = true;
    pcm.capture(new Float32Array(rate / 50).fill(1), (buffer, level) => {
      assert.equal(level, 0); assert.ok(new Uint8Array(buffer).every(byte => byte === 0));
    });
  });
  test(`downlink ${rate} Hz is bounded and produces continuous mono speech`, () => {
    const pcm = new PhonePCM(rate);
    const frame = new ArrayBuffer(320), view = new DataView(frame);
    for (let i = 0; i < 160; i++) view.setInt16(i * 2, -16384, true);
    pcm.enqueue(frame); const waiting = new Float32Array(128); pcm.render(waiting); assert.ok(waiting.every(v => v === 0));
    pcm.enqueue(frame); const playing = new Float32Array(128); pcm.render(playing); assert.ok(playing.every(v => v === -0.5));
    for (let i = 0; i < 100; i++) pcm.enqueue(frame);
    assert.ok(pcm.queue.length <= 2560);
    for (let i = 0; i < 1000; i++) pcm.render(playing);
    assert.ok(playing.every(v => v === 0));
  });
}
test("clipping is safe and microphone is never locally echoed", () => {
  const pcm = new PhonePCM(48000);
  pcm.capture(new Float32Array(960).fill(2), buffer => assert.equal(new DataView(buffer).getInt16(0, true), 32767));
  const output = new Float32Array(128); pcm.render(output); assert.ok(output.every(v => v === 0));
  pcm.enqueue(new ArrayBuffer(3)); pcm.enqueue(new ArrayBuffer(17000)); assert.equal(pcm.queue.length, 0);
});
