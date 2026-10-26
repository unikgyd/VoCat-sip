import { useEffect, useState } from "react";
import { Link, useLocation } from "react-router-dom";
import { Bell, BellOff, Mic, MicOff, Pause, Play, Phone, PhoneIncoming, PhoneOff } from "lucide-react";
import { Button, PageHeader } from "../components/ui";
import { useI18n } from "../lib/i18n";
import { usePhone, type PhoneCall, type PhoneLine } from "./PhoneProvider";

const panel = "rounded-2xl border border-gray-200 bg-white p-5 dark:border-white/10 dark:bg-white/5 dark:text-gray-100";
function StateDot({ ready }: { ready: boolean }) {
  return <span aria-hidden="true" className={`inline-block h-2 w-2 shrink-0 rounded-full ${ready ? "bg-emerald-500" : "bg-amber-500"}`} />;
}
export function CallCard({ line, call }: { line: PhoneLine; call: PhoneCall }) {
  const phone = usePhone(), { t } = useI18n();
  const mine = phone.owned?.deviceId === line.deviceId && phone.owned.callId === call.id;
  const incoming = call.direction === "incoming" && call.state === "ringing";
  const [now, setNow] = useState(Date.now());
  useEffect(() => { const timer = setInterval(() => setNow(Date.now()), 1000); return () => clearInterval(timer); }, []);
  const elapsed = call.answeredAt ? Math.max(0, Math.floor((now - Date.parse(call.answeredAt)) / 1000)) : null;
  const status = call.holdPending ? t("正在更新通话") : call.held ? t("通话已保持") : incoming ? t("来电") : call.state === "active" ? t("通话中") : call.state === "ringing" ? t("对方振铃") : t("正在呼叫");
  return <section className={`${panel} ${incoming ? "ring-1 ring-emerald-500/30" : ""}`} aria-label={`${t("通话")} ${call.number || t("未知号码")}`}>
    <div className="mb-4 flex items-start gap-3">
      <div className={`rounded-xl p-3 ${incoming ? "bg-emerald-50 text-emerald-600 dark:bg-emerald-500/10" : "bg-sky-50 text-sky-600 dark:bg-sky-500/10"}`}><PhoneIncoming size={21} /></div>
      <div className="min-w-0 flex-1"><div className="truncate text-xs text-gray-500">{line.name || line.deviceId} · {status}</div>
        <div className="mt-1 break-all font-mono text-xl font-semibold tracking-tight">{call.number || t("未知号码")}</div>
      </div>
      {elapsed !== null && <span className="shrink-0 font-mono text-sm text-gray-500">{Math.floor(elapsed / 60)}:{String(elapsed % 60).padStart(2, "0")}</span>}
    </div>
    {mine && <div className="mb-4 space-y-2">
      <p className={`text-xs ${phone.audioState === "connected" ? "text-emerald-600" : phone.audioState === "closed" ? "text-red-500" : "text-amber-600"}`}>{t(phone.audioState === "connected" ? "音频已连接" : phone.audioState === "closed" ? "音频已断开" : "等待音频连接")}</p>
      <div className="flex justify-between text-xs text-gray-500"><span>{phone.muted ? t("麦克风已静音") : t("麦克风输入")}</span><span>{call.codec || "PCM 8 kHz"}</span></div>
      <div role="meter" aria-label={t("麦克风输入")} aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(Math.min(1, phone.level * 5) * 100)} className="h-1.5 overflow-hidden rounded-full bg-gray-100 dark:bg-white/10"><div className="h-full bg-emerald-500 transition-[width]" style={{ width: `${Math.min(100, phone.level * 500)}%` }} /></div>
    </div>}
    {!mine && call.audioOwner && <p className="mb-3 text-xs text-gray-500">{t(call.audioOwner === "sip" ? "由 SIP 分机接听" : "由其他网页接听")}</p>}
    <div className="flex flex-wrap gap-2">
      {incoming && !call.audioOwner && <Button variant="success" size="large" icon={<Phone size={16} />} loading={phone.busy} disabled={!!phone.owned || !line.ready} onClick={() => void phone.act("answer", line.deviceId, call.id)}>{t("在本机接听")}</Button>}
      {mine && <Button size="large" icon={phone.muted ? <MicOff size={16} /> : <Mic size={16} />} onClick={phone.toggleMute}>{t(phone.muted ? "取消静音" : "静音")}</Button>}
      {mine && call.canHold && <Button size="large" icon={call.held ? <Play size={16} /> : <Pause size={16} />} loading={phone.holdBusy} disabled={phone.busy || phone.holdBusy || phone.dtmfBusy || call.holdPending} onClick={() => void phone.setHold(!call.held)}>{t(call.held ? "恢复通话" : "保持通话")}</Button>}
      <Button variant="danger" plain size="large" icon={<PhoneOff size={16} />} disabled={phone.busy} onClick={() => void phone.act("hangup", line.deviceId, call.id)}>{t(incoming ? "拒接整通来电" : "挂断")}</Button>
    </div>
    {mine && call.state === "active" && !call.held && <div className="mt-4 border-t border-gray-100 pt-4 dark:border-white/10">
      <p className="mb-3 text-xs text-gray-500">{t(call.dtmfDigits ? "通话按键" : "对方未协商通话按键")}</p>
      {call.dtmfDigits && <div role="group" aria-label={t("通话按键")} className="grid grid-cols-3 gap-2">
        {Array.from("123456789*0#").map(digit => <button key={digit} type="button" aria-label={`${t("发送按键")} ${digit}`}
          disabled={phone.busy || phone.dtmfBusy || phone.holdBusy || call.holdPending || phone.audioState !== "connected" || !call.dtmfDigits?.includes(digit)}
          onClick={() => void phone.sendDTMF(digit)}
          className="h-11 rounded-xl border border-gray-200 font-mono text-lg hover:bg-sky-50 disabled:opacity-40 dark:border-white/15 dark:hover:bg-white/10">{digit}</button>)}
      </div>}
    </div>}
  </section>;
}
function PhoneError() {
  const phone = usePhone(), { t } = useI18n();
  return phone.error ? <div role="alert" className="flex items-start justify-between gap-3 rounded-xl border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-500/20 dark:bg-red-500/10 dark:text-red-300"><span>{phone.error}</span><button type="button" onClick={phone.clearError} className="shrink-0 underline">{t("关闭")}</button></div> : null;
}
export function GlobalPhone() {
  const phone = usePhone(), { t } = useI18n(), location = useLocation();
  if (location.pathname === "/phone") return null;
  const calls = phone.lines.flatMap(line => line.calls.filter(call => call.direction === "incoming" && call.state === "ringing" && !call.audioOwner || phone.owned?.deviceId === line.deviceId && phone.owned.callId === call.id).map(call => ({ line, call })));
  if (!calls.length && !phone.error) return null;
  return <aside aria-label={t("网页电话")} className="fixed bottom-4 right-4 z-40 max-h-[70vh] w-[min(380px,calc(100vw-32px))] space-y-3 overflow-y-auto rounded-2xl shadow-xl">
    <PhoneError />{calls.map(({ line, call }) => <CallCard key={`${line.deviceId}:${call.id}`} line={line} call={call} />)}
    <Link to="/phone" className="block rounded-xl bg-white p-3 text-center text-sm text-sky-600 dark:bg-gray-900">{t("打开网页电话")}</Link>
  </aside>;
}
export default function PhonePage() {
  const phone = usePhone(), { t } = useI18n();
  const [selected, setSelected] = useState("");
  const [number, setNumber] = useState("");
  const line = phone.lines.find(line => line.deviceId === selected) || phone.lines.find(line => line.ready) || phone.lines[0];
  const calls = phone.lines.flatMap(line => line.calls.map(call => ({ line, call })));
  const valid = /^\+[1-9][0-9]{7,14}$/.test(number.trim());
  const secure = window.isSecureContext && !!navigator.mediaDevices;
  return <div className="mx-auto max-w-5xl space-y-5">
    <PageHeader title={t("网页电话")} subtitle={t("每台模组独立运行，来电可在任意页面接听。")}
      actions={<Button icon={phone.chime ? <Bell size={16} /> : <BellOff size={16} />} onClick={phone.toggleChime}>{t(phone.chime ? "提示音已开启" : "开启来电提示音")}</Button>} />
    <PhoneError />
    {!secure && <div role="note" className="rounded-xl border border-amber-200 bg-amber-50 p-4 text-sm text-amber-800 dark:border-amber-500/20 dark:bg-amber-500/10 dark:text-amber-300">{t("网页语音需要 HTTPS 或 localhost")}</div>}
    <div className="grid gap-5 md:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)]">
      <section className={`${panel} self-start`}>
        <h3 className="mb-5 text-base font-semibold">{t("拨打电话")}</h3>
        <form onSubmit={event => { event.preventDefault(); if (line && valid && line.ready && !line.calls.length && !phone.busy && !phone.owned && secure) void phone.act("dial", line.deviceId, number.trim()); }} className="space-y-5">
          <div><label htmlFor="phone-line" className="mb-2 block text-sm text-gray-500">{t("使用线路")}</label>
            <select id="phone-line" value={line?.deviceId || ""} onChange={event => setSelected(event.target.value)} className="h-11 w-full rounded-xl border border-gray-200 bg-transparent px-3 text-sm dark:border-white/15">
              {!phone.lines.length && <option value="">{t(phone.loading ? "加载中" : "暂无模组")}</option>}
              {phone.lines.map(item => <option key={item.deviceId} value={item.deviceId}>{item.name || item.deviceId}</option>)}
            </select>
            {line && <p className="mt-2 flex items-center gap-2 text-xs text-gray-500"><StateDot ready={line.ready} />{t(line.transport === "cellular" ? line.ready ? "模组语音可连接" : "模组语音未就绪" : line.ready ? "IMS 已注册" : "IMS 未就绪")}</p>}
          </div>
          <div><label htmlFor="phone-number" className="mb-2 block text-sm text-gray-500">{t("国际电话号码")}</label>
            <input id="phone-number" type="tel" autoComplete="off" inputMode="tel" maxLength={16} placeholder="+…" value={number} onChange={event => setNumber(event.target.value)} className="h-14 w-full rounded-xl border border-gray-200 bg-transparent px-4 font-mono text-2xl tracking-wide outline-none focus:border-sky-500 dark:border-white/15" />
            <p className="mt-2 text-xs leading-relaxed text-gray-500">{t("请带上 + 和国家码；不会自动补号或改走其他线路。")}</p>
          </div>
          <Button type="submit" block size="large" variant="primary" icon={<Phone size={17} />} loading={phone.busy} disabled={!secure || !valid || !line?.ready || !!line?.calls.length || !!phone.owned}>{t("从所选线路呼叫")}</Button>
          <p className="text-xs leading-relaxed text-gray-500">{t("外呼可能产生运营商费用。麦克风权限确认后才会提交。")}</p>
        </form>
      </section>
      <div className="space-y-4"><h3 className="text-sm font-medium text-gray-500">{t("当前通话")}{calls.length > 0 ? ` · ${calls.length}` : ""}</h3>
        {calls.map(({ line, call }) => <CallCard key={`${line.deviceId}:${call.id}`} line={line} call={call} />)}
        {!calls.length && <div className={`${panel} flex min-h-60 flex-col items-center justify-center text-center`}><div className="mb-4 rounded-full bg-gray-50 p-5 text-gray-300 dark:bg-white/5"><Phone size={30} strokeWidth={1.5} /></div><div className="font-medium">{t("暂无通话")}</div><p className="mt-2 max-w-xs text-sm leading-relaxed text-gray-500">{t("保持网页在线，来电会在所有页面提醒。")}</p></div>}
      </div>
    </div>
    <p className="text-xs leading-relaxed text-gray-400">{t("支持已注册的 IMS 和已配对 USB 音频的模组线路。关闭网页或音频断开会结束本机接管的通话；后台休眠不保证收到提醒。")}</p>
  </div>;
}
