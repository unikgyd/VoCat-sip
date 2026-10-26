import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { api } from "../api";
import { PhoneAudio } from "./audio";
import { tl } from "../lib/i18n";

export interface PhoneCall {
  id: string; number: string; direction: string; state: string; mediaReady?: boolean;
  answeredAt?: string; startedAt: string; audioOwner: string; codec?: string; dtmfDigits?: string;
}
export interface PhoneLine { deviceId: string; name: string; ready: boolean; calls: PhoneCall[]; transport?: string }
interface OwnedCall { deviceId: string; callId: string; token: string; audio: PhoneAudio }
interface PhoneState {
  lines: PhoneLine[]; error: string; loading: boolean; busy: boolean; level: number; muted: boolean; audioState: string;
  owned: { deviceId: string; callId: string } | null;
  act: (action: "dial" | "answer" | "hangup", line: string, target: string) => Promise<void>;
  toggleMute: () => void; clearError: () => void;
  sendDTMF: (digit: string) => Promise<void>; dtmfBusy: boolean;
  chime: boolean; toggleChime: () => void;
}
const Context = createContext<PhoneState | null>(null);
function newPhoneID() { return Array.from(crypto.getRandomValues(new Uint8Array(16)), value => value.toString(16).padStart(2, "0")).join(""); }
export function usePhone() { const context = useContext(Context); if (!context) throw new Error("PhoneProvider missing"); return context; }

export function PhoneProvider({ children }: { children: ReactNode }) {
  const [client] = useState(newPhoneID);
  const [lines, setLines] = useState<PhoneLine[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [level, setLevel] = useState(0);
  const [muted, setMuted] = useState(false);
  const [audioState, setAudioState] = useState("");
  const [owned, setOwned] = useState<PhoneState["owned"]>(null);
  const [chime, setChime] = useState(false);
  const [dtmfBusy, setDTMFBusy] = useState(false);
  const dtmfPending = useRef(false);
  const dtmfOperation = useRef<AbortController | null>(null);
  const chimeContext = useRef<AudioContext | null>(null);
  const current = useRef<OwnedCall | null>(null);
  const preparing = useRef<PhoneAudio | null>(null);
  const pending = useRef(false);
  const active = useRef(true);
  const operation = useRef<AbortController | null>(null);
  const refresh = useRef<() => Promise<void>>(async () => {});
  const headers = { "X-VoCat-Phone-Client": client };

  useEffect(() => {
    active.current = true;
    let stopped = false, timer = 0;
    const controller = new AbortController();
    const load = async () => {
      const ownerAtStart = current.current;
      try {
        const result = await api<{ lines: PhoneLine[] }>("/phone", { headers, signal: AbortSignal.any([controller.signal, AbortSignal.timeout(5000)]), cache: "no-store" });
        if (stopped) return;
        setLines(result.lines);
        const owner = current.current;
        if (owner && owner === ownerAtStart) {
          const call = result.lines.find(line => line.deviceId === owner.deviceId)?.calls.find(call => call.id === owner.callId);
          if (!call) { owner.audio.close(); current.current = null; setOwned(null); setLevel(0); }
          else if (call.state === "active" && call.mediaReady) owner.audio.connect(owner.deviceId, owner.callId, owner.token);
        }
      } catch { if (!stopped) setError(tl("电话状态更新失败，请检查连接")); }
      finally { if (!stopped) setLoading(false); }
    };
    refresh.current = load;
    const loop = async () => { await load(); if (!stopped) timer = window.setTimeout(loop, 1500); };
    void loop();
    return () => {
      stopped = true; active.current = false; clearTimeout(timer); controller.abort(); operation.current?.abort(); dtmfOperation.current?.abort();
      preparing.current?.close(); current.current?.audio.close(); current.current = null;
      void chimeContext.current?.close().catch(() => {}); chimeContext.current = null;
    };
    // The session and client ID last across page navigation, not across logout.
  }, [client]);

  const ringing = lines.some(line => line.calls.some(call => call.state === "ringing" && call.direction === "incoming" && !call.audioOwner));
  useEffect(() => {
    if (!chime || !ringing) return;
    const ring = () => {
      const context = chimeContext.current;
      if (!context || context.state !== "running") return;
      const oscillator = context.createOscillator(), gain = context.createGain();
      oscillator.frequency.value = 520; gain.gain.value = 0;
      oscillator.connect(gain); gain.connect(context.destination);
      const now = context.currentTime;
      gain.gain.linearRampToValueAtTime(0.08, now + 0.02); gain.gain.linearRampToValueAtTime(0, now + 0.35);
      oscillator.start(now); oscillator.stop(now + 0.4);
    };
    ring(); const timer = setInterval(ring, 3500); return () => clearInterval(timer);
  }, [chime, ringing]);

  async function act(action: "dial" | "answer" | "hangup", deviceId: string, target: string) {
    if (pending.current) return;
    pending.current = true; setBusy(true); setError("");
    const abort = operation.current = new AbortController();
    let audio: PhoneAudio | null = null;
    try {
      if (action !== "hangup") {
        if (current.current) throw new Error(tl("本机已有通话，请先挂断"));
        audio = preparing.current = new PhoneAudio();
        let levelAt = 0;
        audio.onLevel = value => { if (active.current && performance.now() - levelAt > 100) { levelAt = performance.now(); setLevel(value); } };
        audio.onError = value => { if (active.current) setError(tl(value)); };
        audio.onState = value => { if (active.current) setAudioState(value); };
        await audio.prepare(); // Do not answer or dial before permission succeeds.
        if (!active.current || abort.signal.aborted) throw new Error(tl("操作已取消"));
      }
      const result = await api<{ call?: PhoneCall; audioToken?: string }>(`/phone/${action}`, {
        method: "POST", headers, signal: AbortSignal.any([abort.signal, AbortSignal.timeout(35000)]),
        body: { deviceId, ...(action === "dial" ? { number: target, operationId: newPhoneID() } : { callId: target }) },
      });
      if (!active.current) { audio?.close(); return; }
      if (audio && (!result.call?.id || !result.audioToken)) throw new Error(tl("通话响应不完整，请核对状态；不会自动重拨"));
      if (audio && result.call && result.audioToken) {
        current.current = { deviceId, callId: result.call.id, token: result.audioToken, audio };
        preparing.current = null;
        setOwned({ deviceId, callId: result.call.id }); setMuted(false);
        if (result.call.state === "active" && result.call.mediaReady) audio.connect(deviceId, result.call.id, result.audioToken);
      } else if (action === "hangup" && current.current?.deviceId === deviceId && current.current.callId === target) {
        current.current.audio.close(); current.current = null; setOwned(null); setLevel(0);
      }
      await refresh.current();
    } catch (err) {
      audio?.close(); preparing.current = null;
      if (active.current) setError(err instanceof DOMException && err.name === "NotAllowedError" ? tl("请允许麦克风权限，通话尚未提交") : err instanceof Error ? tl(err.message) : tl("操作失败，请检查通话状态后重试"));
    } finally {
      pending.current = false;
      if (active.current) setBusy(false);
    }
  }
  async function sendDTMF(digit: string) {
    const owner = current.current;
    if (!owner || dtmfPending.current || !/^[0-9*#A-D]$/.test(digit)) return;
    dtmfPending.current = true; setDTMFBusy(true); setError("");
    const abort = dtmfOperation.current = new AbortController();
    try {
      await api("/phone/dtmf", { method: "POST", headers,
        signal: AbortSignal.any([abort.signal, AbortSignal.timeout(2500)]),
        body: { deviceId: owner.deviceId, callId: owner.callId, digit },
      });
    } catch {
      if (active.current && current.current === owner) setError(tl("按键发送失败或结果未知，不会自动重发"));
    } finally {
      dtmfPending.current = false;
      if (active.current) setDTMFBusy(false);
    }
  }
  return <Context.Provider value={{ lines, error, loading, busy, level, muted, owned, audioState, act, sendDTMF, dtmfBusy,
    chime, toggleChime: () => {
      if (chime) { setChime(false); return; }
      try {
        chimeContext.current ??= new AudioContext();
        void chimeContext.current.resume().then(() => setChime(true)).catch(() => setError(tl("浏览器未允许提示音")));
      } catch { setError(tl("浏览器未允许提示音")); }
    },
    toggleMute: () => { if (current.current) setMuted(current.current.audio.toggleMute()); },
    clearError: () => setError("") }}>{children}</Context.Provider>;
}
