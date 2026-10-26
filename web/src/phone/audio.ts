export class PhoneAudio {
  private context: AudioContext | null = null;
  private stream: MediaStream | null = null;
  private node: AudioWorkletNode | null = null;
  private socket: WebSocket | null = null;
  private stopped = false;
  private connectTimer = 0;
  connected = false;
  muted = false;
  onLevel: (level: number) => void = () => {};
  onError: (error: string) => void = () => {};
  onState: (state: "waiting" | "connecting" | "connected" | "closed") => void = () => {};

  async prepare() {
    if (!window.isSecureContext || !navigator.mediaDevices?.getUserMedia || !window.AudioWorkletNode) {
      throw new Error("网页语音需要 HTTPS 或 localhost");
    }
    this.context = new AudioContext();
    await this.context.resume();
    this.stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true }, video: false });
    if (this.stopped) { this.stream.getTracks().forEach(track => track.stop()); throw new Error("操作已取消"); }
    await this.context.audioWorklet.addModule("/phone-pcm.js");
    if (this.stopped) throw new Error("操作已取消");
    this.node = new AudioWorkletNode(this.context, "vocat-phone-pcm", { numberOfInputs: 1, numberOfOutputs: 1, outputChannelCount: [1] });
    this.context.createMediaStreamSource(this.stream).connect(this.node);
    this.node.connect(this.context.destination);
    this.node.onprocessorerror = () => { this.onError("音频处理已停止，本机通话将结束"); this.close(); };
    this.context.onstatechange = () => {
      if (!this.stopped && this.context?.state !== "running") { this.onError("浏览器暂停了音频，本机通话将结束"); this.close(); }
    };
    this.onState("waiting");
    this.node.port.onmessage = ({ data }: MessageEvent<{ pcm: ArrayBuffer; level: number }>) => {
      this.onLevel(data.level);
      // At most 100 ms of queued PCM. Never replay delayed microphone speech.
      if (this.socket?.readyState === WebSocket.OPEN && this.socket.bufferedAmount <= 1600) this.socket.send(data.pcm);
    };
  }
  connect(device: string, call: string, token: string) {
    if (this.stopped || this.socket) return;
    const url = new URL("/api/phone/media", location.href);
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    url.searchParams.set("call_id", call);
    url.searchParams.set("device_id", device);
    const ws = this.socket = new WebSocket(url, ["vocat-pcm", `lease-${token}`]);
    this.onState("connecting");
    this.connectTimer = window.setTimeout(() => { if (!this.stopped && ws.readyState !== WebSocket.OPEN) { this.onError("音频连接失败，请检查 HTTPS 和网络"); this.close(); } }, 10000);
    ws.binaryType = "arraybuffer";
    ws.onopen = () => { clearTimeout(this.connectTimer); this.connected = true; this.onState("connected"); };
    ws.onmessage = ({ data }) => { if (data instanceof ArrayBuffer) this.node?.port.postMessage(data, [data]); };
    ws.onclose = () => {
      this.connected = false;
      if (!this.stopped) { this.onError("音频连接已断开，本机通话将结束"); this.close(); }
    };
    ws.onerror = () => { if (!this.stopped) this.onError("音频连接失败，请检查 HTTPS 和网络"); };
  }
  toggleMute() {
    this.muted = !this.muted;
    this.stream?.getAudioTracks().forEach(track => { track.enabled = !this.muted; });
    this.node?.port.postMessage({ muted: this.muted });
    return this.muted;
  }
  close() {
    this.stopped = true; this.connected = false;
    clearTimeout(this.connectTimer); this.onState("closed");
    this.socket?.close(); this.socket = null;
    this.node?.disconnect(); this.node = null;
    this.stream?.getTracks().forEach(track => track.stop()); this.stream = null;
    void this.context?.close().catch(() => {}); this.context = null;
  }
}
