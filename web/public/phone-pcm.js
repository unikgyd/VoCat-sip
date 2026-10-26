// Self-hosted AudioWorklet, also imported by deterministic Node tests.
// No microphone samples are returned to the speakers.
export class PhonePCM {
  constructor(rate) {
    this.rate = rate; this.ratio = rate / 8000;
    this.sum = 0; this.weight = 0; this.frame = [];
    this.queue = []; this.phase = 0; this.playing = false; this.muted = false;
  }
  capture(input, emit) {
    for (const value of input) {
      let left = 1;
      while (left > 1e-8) {
        const take = Math.min(left, this.ratio - this.weight);
        this.sum += (this.muted ? 0 : value) * take; this.weight += take; left -= take;
        if (this.weight >= this.ratio - 1e-8) {
          this.frame.push(Math.round(Math.max(-1, Math.min(1, this.sum / this.weight)) * 32767));
          this.sum = 0; this.weight = 0;
          if (this.frame.length === 160) {
            const buffer = new ArrayBuffer(320), view = new DataView(buffer);
            let energy = 0;
            this.frame.forEach((s, i) => { view.setInt16(i * 2, s, true); energy += s * s; });
            emit(buffer, Math.sqrt(energy / 160) / 32768);
            this.frame = [];
          }
        }
      }
    }
  }
  enqueue(buffer) {
    if (!(buffer instanceof ArrayBuffer) || buffer.byteLength % 2 || buffer.byteLength > 16384) return;
    const view = new DataView(buffer);
    for (let i = 0; i < buffer.byteLength; i += 2) this.queue.push(view.getInt16(i, true) / 32768);
    if (this.queue.length > 2560) { this.queue.splice(0, this.queue.length - 640); this.phase = 0; }
  }
  render(output) {
    output.fill(0);
    if (!this.playing && this.queue.length >= 320) this.playing = true;
    if (!this.playing) return;
    let consumed = 0;
    for (let i = 0; i < output.length; i++) {
      const index = Math.floor(this.phase), fraction = this.phase - index;
      if (index + 1 >= this.queue.length) { this.playing = false; break; }
      output[i] = this.queue[index] * (1 - fraction) + this.queue[index + 1] * fraction;
      this.phase += 8000 / this.rate;
      consumed = Math.floor(this.phase);
    }
    if (consumed) { this.queue.splice(0, consumed); this.phase -= consumed; }
  }
}

if (typeof registerProcessor === "function") {
  class PhoneProcessor extends AudioWorkletProcessor {
    constructor() {
      super(); this.pcm = new PhonePCM(sampleRate);
      this.port.onmessage = ({ data }) => {
        if (data instanceof ArrayBuffer) this.pcm.enqueue(data);
        else if (typeof data?.muted === "boolean") this.pcm.muted = data.muted;
      };
    }
    process(inputs, outputs) {
      this.pcm.render(outputs[0][0]);
      if (inputs[0]?.[0]) this.pcm.capture(inputs[0][0], (pcm, level) => this.port.postMessage({ pcm, level }, [pcm]));
      return true;
    }
  }
  registerProcessor("vocat-phone-pcm", PhoneProcessor);
}
