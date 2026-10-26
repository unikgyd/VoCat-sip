import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import ts from "typescript";

const source = await readFile(new URL("../src/phone/audio.ts", import.meta.url), "utf8");
const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ES2022, target: ts.ScriptTarget.ES2022 } });
const { PhoneAudio } = await import(`data:text/javascript;base64,${Buffer.from(compiled.outputText).toString("base64")}`);
function environment(getMedia) {
  const state = { stops: 0, contextsClosed: 0, frames: [], socket: null, node: null };
  const track = { enabled: true, stop() { state.stops++; } };
  const stream = { getTracks: () => [track], getAudioTracks: () => [track] };
  class Context {
    state = "running";
    audioWorklet = { addModule: async () => {} };
    resume = async () => {};
    createMediaStreamSource() { return { connect() {} }; }
    close = async () => { state.contextsClosed++; this.state = "closed"; };
  }
  class Node {
    constructor() { state.node = this; }
    port = { postMessage() {} };
    connect() {} disconnect() {}
  }
  class Socket {
    static OPEN = 1;
    readyState = 1; bufferedAmount = 0;
    constructor(url, protocols) { state.socket = this; this.url = url; this.protocols = protocols; }
    send(buffer) { state.frames.push(buffer); }
    close() { this.readyState = 3; }
  }
  Object.defineProperty(globalThis, "navigator", { configurable: true, value: { mediaDevices: { getUserMedia: getMedia || (async () => stream) } } });
  Object.assign(globalThis, { window: { isSecureContext: true, AudioWorkletNode: Node, setTimeout, clearTimeout }, AudioContext: Context, AudioWorkletNode: Node, WebSocket: Socket, location: new URL("https://voice.example.test/phone") });
  return { state, stream, track };
}
test("audio uses an in-memory subprotocol lease, bounded frames and true mute", async () => {
  const { state, track } = environment(), audio = new PhoneAudio();
  await audio.prepare(); audio.connect("module a", "exact-call", "secret-token");
  assert.ok(!state.socket.url.href.includes("secret-token"));
  assert.deepEqual(state.socket.protocols, ["vocat-pcm", "lease-secret-token"]);
  state.socket.onopen(); assert.equal(audio.connected, true);
  state.node.port.onmessage({ data: { pcm: new ArrayBuffer(320), level: 0.2 } });
  assert.equal(state.frames.length, 1);
  state.socket.bufferedAmount = 1601;
  state.node.port.onmessage({ data: { pcm: new ArrayBuffer(320), level: 0.2 } });
  assert.equal(state.frames.length, 1);
  assert.equal(audio.toggleMute(), true); assert.equal(track.enabled, false);
  assert.equal(audio.toggleMute(), false); assert.equal(track.enabled, true);
  audio.close(); assert.equal(state.stops, 1); assert.equal(state.contextsClosed, 1);
});
test("late microphone permission after logout immediately stops the track", async () => {
  let grant;
  const { state, stream } = environment(() => new Promise(resolve => { grant = resolve; }));
  const audio = new PhoneAudio(), ready = audio.prepare();
  await Promise.resolve(); audio.close(); grant(stream);
  await assert.rejects(ready, /取消/);
  assert.equal(state.stops, 1); assert.equal(state.contextsClosed, 1);
});
test("insecure origin never asks for a microphone", async () => {
  let requested = 0; environment(async () => { requested++; }); window.isSecureContext = false;
  await assert.rejects(new PhoneAudio().prepare(), /HTTPS/); assert.equal(requested, 0);
});
test("socket loss and worklet failure stop microphone and output", async () => {
  for (const mode of ["socket", "processor"]) {
    const { state } = environment(), audio = new PhoneAudio(); let error = ""; audio.onError = value => { error = value; };
    await audio.prepare(); audio.connect("a", "call", "token");
    if (mode === "socket") state.socket.onclose(); else state.node.onprocessorerror();
    assert.ok(error); assert.equal(state.stops, 1); assert.equal(state.contextsClosed, 1); assert.equal(audio.connected, false);
  }
});
