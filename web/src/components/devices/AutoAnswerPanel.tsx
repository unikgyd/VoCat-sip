import { useEffect, useId, useRef, useState } from "react";
import { api, apiMessage } from "../../api";
import { useI18n } from "../../lib/i18n";
import { Button, message } from "../ui";
import { Switch } from "../ui/Switch";
import type { DeviceDetail } from "./types";

interface AnswerSettings {
  iccid: string; revision: string; enabled: boolean; delaySeconds: number;
  hangupAfterPlayback: boolean; maxCallSeconds: number;
  prompt: { name: string; durationMs: number; format: string } | null;
}
interface AnswerResponse { settings: AnswerSettings | null; compressedAvailable: boolean }

export function AutoAnswerPanel({ device }: { device: DeviceDetail }) {
  const { t } = useI18n(), id = useId();
  const [saved, setSaved] = useState<AnswerSettings | null>(null);
  const [draft, setDraft] = useState<AnswerSettings | null>(null);
  const [compressed, setCompressed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [reload, setReload] = useState(0);
  const epoch = useRef(0);
  const base = `/devices/${encodeURIComponent(device.id)}/auto-answer`;
  const sim = device.modem?.simInserted === false ? "" : device.modem?.iccid || device.cellularImsRuntime?.iccid || device.vowifiRuntime?.iccid || "";
  useEffect(() => {
    const sequence = ++epoch.current, abort = new AbortController();
    setSaved(null); setDraft(null); setError(""); setBusy(false);
    api<AnswerResponse>(base, { signal: abort.signal, cache: "no-store" }).then(result => {
      if (epoch.current === sequence) { setSaved(result.settings); setDraft(result.settings); setCompressed(result.compressedAvailable); }
    }).catch(err => { if (!abort.signal.aborted && epoch.current === sequence) setError(apiMessage(err) || t("自动接听设置加载失败")); });
    return () => { ++epoch.current; abort.abort(); };
  }, [base, sim, reload]);
  const changed = JSON.stringify(draft) !== JSON.stringify(saved);
  const update = (patch: Partial<AnswerSettings>) => setDraft(value => value ? { ...value, ...patch } : value);
  const commit = async (method: "PUT" | "POST" | "DELETE", body: unknown, prompt = false) => {
    const sequence = epoch.current;
    setBusy(true); setError("");
    try {
      const result = await api<AnswerResponse>(`${base}${prompt ? "/prompt" : ""}`, { method, body, signal: AbortSignal.timeout(60000) });
      if (sequence !== epoch.current) return;
      setSaved(result.settings); setDraft(result.settings); setCompressed(result.compressedAvailable);
      message.success(t("自动接听设置已保存"));
    } catch (err) { if (sequence === epoch.current) setError(apiMessage(err) || t("自动接听设置保存失败")); }
    finally { if (sequence === epoch.current) setBusy(false); }
  };
  const upload = (file?: File) => {
    if (!saved || !file || busy) return;
    if (!file.size || file.size > 64 * 1024 * 1024) { setError(t("留言文件不能为空且不能超过 64 MiB")); return; }
    const form = new FormData(); form.append("iccid", saved.iccid); form.append("revision", saved.revision); form.append("file", file);
    void commit("POST", form, true);
  };
  const inputClass = "w-full rounded-lg border border-gray-200 bg-transparent px-3 py-2 text-sm dark:border-white/15";
  return <section className="ui-panel-muted p-4" aria-labelledby={`${id}-title`} aria-busy={busy}>
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div><h3 id={`${id}-title`} className="text-sm font-semibold">{t("自动接听与留言")}</h3>
        <p className="mt-1 text-xs text-gray-500">{t("按 SIM 保存。延时后仍为原来电才接听；网页或 SIP 分机先接听时停止自动应答。")}</p></div>
      <Button loading={busy} disabled={!draft || !changed} onClick={() => draft && void commit("PUT", draft)}>{t("保存")}</Button>
    </div>
    {error && <div className="mt-3 text-sm text-red-600" role="alert">{error} <Button variant="text" disabled={busy} onClick={() => setReload(v => v + 1)}>{t("重新加载")}</Button></div>}
    {!draft ? <p className="mt-3 text-xs text-gray-500">{t("等待识别 SIM 卡及自动接听设置")}</p> : <>
      <div className="mt-4 flex items-center justify-between gap-3"><span className="text-sm">{t("启用自动接听")}</span>
        <Switch checked={draft.enabled} disabled={busy || !draft.prompt} ariaLabel={t("启用自动接听")} onChange={enabled => update({ enabled })} /></div>
      <div className="mt-4 grid gap-4 sm:grid-cols-3">
        <label className="space-y-1 text-xs text-gray-500" htmlFor={`${id}-delay`}><span>{t("接听延时（秒）")}</span>
          <input id={`${id}-delay`} className={inputClass} type="number" min={5} max={120} step={1} value={draft.delaySeconds} disabled={busy} onChange={e => update({ delaySeconds: Number(e.target.value) })} /></label>
        <label className="space-y-1 text-xs text-gray-500" htmlFor={`${id}-after`}><span>{t("留言播放后")}</span>
          <select id={`${id}-after`} className={inputClass} value={draft.hangupAfterPlayback ? "hangup" : "wait"} disabled={busy} onChange={e => update({ hangupAfterPlayback: e.target.value === "hangup" })}>
            <option value="hangup">{t("挂断原通话")}</option><option value="wait">{t("等待网页接管")}</option></select></label>
        <label className="space-y-1 text-xs text-gray-500" htmlFor={`${id}-max`}><span>{t("自动通话最长秒数")}</span>
          <input id={`${id}-max`} className={inputClass} type="number" min={30} max={3600} step={1} value={draft.maxCallSeconds} disabled={busy} onChange={e => update({ maxCallSeconds: Number(e.target.value) })} /></label>
      </div>
      <div className="mt-4 rounded-lg border border-gray-200 p-3 dark:border-white/10">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm">{draft.prompt ? `${draft.prompt.name} · ${(draft.prompt.durationMs / 1000).toFixed(1)} s` : t("请先上传留言音频")}</p>
          <div className="flex flex-wrap items-center gap-2"><label className={`rounded-lg border border-gray-200 px-3 py-2 text-xs dark:border-white/15 ${busy || changed ? "opacity-40" : "cursor-pointer"}`}>
            {t("上传留言")}<input type="file" className="sr-only" accept={compressed ? ".wav,.mp3,.wma,.aac,.m4a" : ".wav"} disabled={busy || changed} onChange={e => { upload(e.target.files?.[0]); e.target.value = ""; }} /></label>
            {saved?.prompt && <Button size="small" disabled={busy || changed} onClick={() => void commit("DELETE", saved, true)}>{t("删除留言")}</Button>}</div>
        </div>
        {saved?.prompt && <audio key={saved.revision} className="mt-3 h-9 w-full" controls preload="none" aria-label={t("试听留言")} src={`/api${base}/prompt?iccid=${encodeURIComponent(saved.iccid)}&revision=${encodeURIComponent(saved.revision)}`} />}
        <p className="mt-2 text-xs text-gray-500">{t(compressed ? "支持 WAV、MP3、WMA、AAC，最长 5 分钟、64 MiB。" : "支持 WAV，最长 5 分钟、64 MiB；安装 FFmpeg 后可导入 MP3、WMA、AAC。")}</p>
        <p className="mt-1 text-xs text-gray-500">{t("替换或删除留言会关闭自动接听。先保存参数，再上传；播放期间可从网页电话接管。")}</p>
      </div>
      <p className="mt-3 text-xs text-gray-500">{t("等待接管期间发送静音，达到时限会挂断；接管后使用网页麦克风，不再受自动通话时限控制。")}</p>
    </>}
  </section>;
}
