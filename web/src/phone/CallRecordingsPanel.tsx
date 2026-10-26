import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, apiMessage } from "../api";
import { useI18n } from "../lib/i18n";
import { Button, confirmDialog } from "../components/ui";
import { recordingAudio, type CallRecording, type RecordingStatus } from "./recordings";

export function CallRecordingsPanel() {
  const { t } = useI18n();
  const [rows, setRows] = useState<CallRecording[]>([]);
  const [status, setStatus] = useState<RecordingStatus | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [playing, setPlaying] = useState("");
  const [reload, setReload] = useState(0);
  const [page, setPage] = useState(0);
  const [device, setDevice] = useState("");
  useEffect(() => {
    let stopped = false, timer = 0;
    const controller = new AbortController();
    const load = async () => {
      try {
        const result = await api<{ recordings: CallRecording[]; status: RecordingStatus }>("/recordings", { cache: "no-store", signal: AbortSignal.any([controller.signal, AbortSignal.timeout(5000)]) });
        if (!stopped) { setRows(result.recordings); setStatus(result.status); setError(""); }
      } catch (err) { if (!stopped) setError(apiMessage(err) || t("录音列表加载失败")); }
      if (!stopped) timer = window.setTimeout(load, 10000);
    };
    void load(); return () => { stopped = true; clearTimeout(timer); controller.abort(); };
  }, [reload]);
  const remove = async (id?: string) => {
    if (!await confirmDialog(t(id ? "删除这通录音？此操作不可恢复。" : "清理全部已结束录音？正在录音的通话会保留。"), t("通话录音"), { type: "danger" })) return;
    setBusy(true);
    try {
      const result = await api<{ failed?: number }>(`/recordings${id ? `/${encodeURIComponent(id)}` : ""}`, { method: "DELETE", ...(id ? {} : { body: { allCompleted: true } }) });
      if (result.failed) throw new Error(t("部分录音清理失败，请检查存储后重试"));
      setPlaying(""); setReload(value => value + 1); setError("");
    } catch (err) { setError(apiMessage(err) || t("录音删除失败")); }
    finally { setBusy(false); }
  };
  const devices = [...new Set(rows.map(row => row.deviceId).filter(Boolean))];
  const filtered = device ? rows.filter(row => row.deviceId === device) : rows;
  const pages = Math.max(1, Math.ceil(filtered.length / 20)), currentPage = Math.min(page, pages - 1);
  return <section className="rounded-2xl border border-gray-200 bg-white p-5 dark:border-white/10 dark:bg-white/5" aria-label={t("通话录音")}>
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div><h3 className="font-semibold">{t("通话录音")}</h3><p className="mt-1 text-xs text-gray-500">{t("双声道 WAV：左声道为发送，右声道为接收。")}</p></div>
      <div className="flex flex-wrap items-center gap-2"><Link to="/settings" className="text-xs text-sky-600">{t("录音设置")}</Link><Button size="small" disabled={busy} onClick={() => setReload(v => v + 1)}>{t("刷新")}</Button>
        <Button size="small" variant="danger" plain disabled={busy || !rows.some(row => row.state !== "recording")} onClick={() => void remove()}>{t("清理已结束录音")}</Button></div>
    </div>
    {status && <p className="mt-3 text-xs text-gray-500">{t(status.config.enabled ? "录音已开启" : "录音已关闭")} · {(status.usedBytes / 1048576).toFixed(1)} / {status.config.maxStorageMib} MiB · {t("正在录音")} {status.active}</p>}
    {(error || status?.lastError) && <p role="alert" className="mt-3 text-sm text-red-600">{error || status?.lastError}</p>}
    {devices.length > 1 && <select className="mt-3 rounded-lg border border-gray-200 bg-transparent px-3 py-2 text-sm dark:border-white/15" aria-label={t("筛选录音设备")} value={device} onChange={e => { setDevice(e.target.value); setPage(0); }}><option value="">{t("全部设备")}</option>{devices.map(id => <option key={id} value={id}>{id}</option>)}</select>}
    <div className="mt-3 max-h-[32rem] space-y-3 overflow-y-auto">
      {filtered.slice(currentPage * 20, (currentPage + 1) * 20).map(row => <article key={row.id} className="rounded-xl border border-gray-100 p-3 dark:border-white/10">
        <div className="flex flex-wrap items-start justify-between gap-3"><div className="min-w-0 text-sm"><p className="break-all font-mono">{row.number || t("未知号码")}</p>
          <p className="mt-1 break-all text-xs text-gray-500">{row.deviceId || t("设备未知")} · {row.access || "—"} · {t(row.direction === "incoming" ? "来电" : "呼出")} · {new Date(row.startedAt).toLocaleString()}</p>
          <p className="mt-1 text-xs text-gray-500">{t(row.state === "recording" ? "正在录音" : row.state === "complete" ? "录音已保存" : row.state === "interrupted" ? "进程中断的录音" : row.state === "truncated" ? "达到录音上限" : "录音不完整")} · {(row.durationMs / 1000).toFixed(1)} s{row.stopReason && row.state !== "complete" ? ` · ${row.stopReason}` : ""}</p>
          {row.droppedSamples > 0 && <p className="mt-1 text-xs text-amber-600">{t("存在录音丢帧")}: {row.droppedSamples}</p>}</div>
          {row.state !== "recording" && <div className="flex items-center gap-2 text-xs"><Button size="small" disabled={busy} onClick={() => setPlaying(value => value === row.id ? "" : row.id)}>{t(playing === row.id ? "关闭试听" : "试听")}</Button>
            <a className="text-sky-600" download={`call_${row.id}.wav`} href={`${recordingAudio(row.id)}?download=1`}>{t("下载")}</a>
            <Button size="small" variant="danger" plain disabled={busy} onClick={() => void remove(row.id)}>{t("删除")}</Button></div>}</div>
        {playing === row.id && row.state !== "recording" && <audio className="mt-3 h-9 w-full" controls preload="metadata" src={recordingAudio(row.id)} aria-label={t("试听通话录音")} />}
      </article>)}
      {!filtered.length && !error && <p className="py-4 text-sm text-gray-500">{t("暂无通话录音")}</p>}
    </div>
    {pages > 1 && <div className="mt-3 flex items-center justify-end gap-3 text-xs"><Button size="small" disabled={!currentPage} onClick={() => setPage(currentPage - 1)}>{t("上一页")}</Button><span>{currentPage + 1} / {pages}</span><Button size="small" disabled={currentPage + 1 >= pages} onClick={() => setPage(currentPage + 1)}>{t("下一页")}</Button></div>}
  </section>;
}
