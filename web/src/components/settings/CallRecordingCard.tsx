import { useEffect, useId, useState } from "react";
import { Link } from "react-router-dom";
import { api, apiMessage } from "../../api";
import { useI18n } from "../../lib/i18n";
import { Button, message } from "../ui";
import { Switch } from "../ui/Switch";
import type { RecordingConfig, RecordingStatus } from "../../phone/recordings";

export function CallRecordingCard() {
  const { t } = useI18n(), id = useId();
  const [status, setStatus] = useState<RecordingStatus | null>(null);
  const [draft, setDraft] = useState<RecordingConfig | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [reload, setReload] = useState(0);
  useEffect(() => {
    let stale = false; const abort = new AbortController();
    api<RecordingStatus>("/settings/recordings", { signal: abort.signal }).then(value => { if (!stale) { setStatus(value); setDraft(value.config); setError(""); } }).catch(err => { if (!stale) setError(apiMessage(err) || t("录音设置加载失败")); });
    return () => { stale = true; abort.abort(); };
  }, [reload]);
  const save = async () => {
    if (!draft) return; setBusy(true);
    try { const value = await api<RecordingStatus>("/settings/recordings", { method: "PUT", body: draft }); setStatus(value); setDraft(value.config); setError(""); message.success(t("录音设置已保存")); }
    catch (err) { setError(apiMessage(err) || t("录音设置保存失败")); }
    finally { setBusy(false); }
  };
  const update = (patch: Partial<RecordingConfig>) => setDraft(value => value ? { ...value, ...patch } : value);
  const inputClass = "mt-1 w-full rounded-lg border border-gray-200 bg-transparent px-3 py-2 text-sm dark:border-white/15";
  return <section className="ui-card p-8" aria-labelledby={`${id}-title`} aria-busy={busy}>
    <div className="flex flex-wrap items-center justify-between gap-3"><div><h3 id={`${id}-title`} className="font-semibold">{t("通话录音")}</h3><p className="mt-1 text-xs text-gray-500">{t("统一保存 IMS 与 USB 模组通话，包括网页、SIP 和自动应答。")}</p></div>
      <Button disabled={!draft || JSON.stringify(draft) === JSON.stringify(status?.config)} loading={busy} onClick={() => void save()}>{t("保存")}</Button></div>
    {error && <p role="alert" className="mt-3 text-sm text-red-600">{error} <Button variant="text" disabled={busy} onClick={() => setReload(v => v + 1)}>{t("重新加载")}</Button></p>}
    {draft && <><div className="mt-4 flex items-center justify-between gap-3"><span className="text-sm">{t("保存通话录音")}</span><Switch ariaLabel={t("保存通话录音")} checked={draft.enabled} disabled={busy} onChange={enabled => update({ enabled })} /></div>
      <div className="mt-4 grid gap-4 sm:grid-cols-3">
        <label className="text-xs text-gray-500" htmlFor={`${id}-keep`}>{t("保留最近通数")}<input id={`${id}-keep`} className={inputClass} type="number" min={1} max={1000} value={draft.keepCalls} disabled={busy} onChange={e => update({ keepCalls: Number(e.target.value) })} /></label>
        <label className="text-xs text-gray-500" htmlFor={`${id}-minutes`}>{t("单通录音分钟上限")}<input id={`${id}-minutes`} className={inputClass} type="number" min={1} max={1440} value={draft.maxMinutes} disabled={busy} onChange={e => update({ maxMinutes: Number(e.target.value) })} /></label>
        <label className="text-xs text-gray-500" htmlFor={`${id}-quota`}>{t("录音存储上限（MiB）")}<input id={`${id}-quota`} className={inputClass} type="number" min={16} max={16384} value={draft.maxStorageMib} disabled={busy} onChange={e => update({ maxStorageMib: Number(e.target.value) })} /></label>
      </div><p className="mt-3 text-xs leading-relaxed text-gray-500">{t("启用对后续接通的通话生效；关闭立即停止当前录音。达到保留或容量上限时清理最旧的已结束录音，录音失败不影响通话。")}</p>
      <p className="mt-2 break-all text-xs text-gray-500">{t("录音目录")}: {status?.directory}</p><p className="mt-1 text-xs text-gray-500">{t("目录可在服务器配置中修改，重启后生效。")}</p>
      <Link className="mt-3 inline-block text-sm text-sky-600" to="/phone">{t("查看与下载通话录音")}</Link></>}
  </section>;
}
