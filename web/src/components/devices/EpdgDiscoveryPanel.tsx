import { useEffect, useId, useRef, useState } from "react";
import { api, apiMessage } from "../../api";
import { useI18n } from "../../lib/i18n";
import { Button, Textarea, message } from "../ui";
import { Switch } from "../ui/Switch";

interface Configuration { preferredAddresses: string[]; additionalPlmns: string[]; manualOnly: boolean; geographicDnsFallback: boolean; simToken: string }
interface Form { hosts: string; plmns: string; manualOnly: boolean; geographicDnsFallback: boolean; simToken: string }
const tokens = (value: string) => [...new Set(value.split(/[\s,;]+/).filter(Boolean))];
export function EpdgDiscoveryPanel({ deviceId, simKey }: { deviceId: string; simKey: string }) {
  const { t } = useI18n(); const id = useId();
  const [loaded, setLoaded] = useState(false);
  return <details className="ui-panel-muted p-4" onToggle={e => { if (e.currentTarget.open) setLoaded(true); }}>
    <summary className="cursor-pointer text-sm font-medium">{t("高级 · ePDG 发现")}</summary>
    {loaded && <EpdgForm key={`${deviceId}:${simKey}`} deviceId={deviceId} simKey={simKey} id={id} />}
  </details>;
}
function EpdgForm({ deviceId, simKey, id }: { deviceId: string; simKey: string; id: string }) {
  const { t } = useI18n(); const mounted = useRef(true);
  const [form, setForm] = useState<Form | null>(null); const [saved, setSaved] = useState("");
  const [error, setError] = useState(false); const [busy, setBusy] = useState(false); const [retry, setRetry] = useState(0);
  const adopt = (data: Configuration) => { const next = { hosts: (data.preferredAddresses || []).join("\n"), plmns: (data.additionalPlmns || []).join("\n"), manualOnly: data.manualOnly === true, geographicDnsFallback: data.geographicDnsFallback === true, simToken: data.simToken }; setForm(next); setSaved(JSON.stringify(next)); };
  useEffect(() => {
    mounted.current = true; let active = true; const controller = new AbortController();
    setError(false); if (simKey) api<Configuration>(`/devices/${encodeURIComponent(deviceId)}/vowifi/discovery`, { signal: AbortSignal.any([controller.signal, AbortSignal.timeout(10000)]) }).then(data => { if (active) adopt(data); }).catch(() => { if (active) setError(true); });
    return () => { active = false; mounted.current = false; controller.abort(); };
  }, [deviceId, simKey, retry]);
  const save = async () => {
    if (!form) return; const hosts = tokens(form.hosts), plmns = tokens(form.plmns);
    if (hosts.length > 16 || plmns.length > 16 || (form.manualOnly && !hosts.length)) { message.error(t("最多 16 个地址和 16 个 PLMN；仅手动模式必须填写地址。")); return; }
    if (plmns.some(p => !/^\d{3}-?\d{2,3}$/.test(p))) { message.error(t("PLMN 格式为 MCC-MNC 或五／六位数字。")); return; }
    setBusy(true);
    try { const data = await api<Configuration>(`/devices/${encodeURIComponent(deviceId)}/vowifi/discovery`, { method: "PUT", signal: AbortSignal.timeout(10000), body: { preferredAddresses: hosts, additionalPlmns: plmns, manualOnly: form.manualOnly, geographicDnsFallback: form.geographicDnsFallback, simToken: form.simToken } }); if (mounted.current) { adopt(data); message.success(t("ePDG 配置已保存，下次连接生效")); } }
    catch (err) { if (mounted.current) message.error(apiMessage(err) || t("ePDG 配置保存失败")); }
    finally { if (mounted.current) setBusy(false); }
  };
  const preview = form ? tokens(form.plmns).filter(p => /^\d{3}-?\d{2,3}$/.test(p)).map(p => p.replace("-", "")).map(p => `epdg.epc.mnc${p.slice(3).padStart(3, "0")}.mcc${p.slice(0, 3)}.pub.3gppnetwork.org`) : [];
  return <div className="mt-4 space-y-4">
    {!simKey ? <p className="text-xs text-gray-500">{t("识别 SIM 后可配置此卡的 ePDG。")}</p> : error ? <div role="alert" className="flex items-center justify-between gap-3 text-xs text-red-500"><span>{t("ePDG 配置加载失败")}</span><Button size="small" onClick={() => setRetry(v => v + 1)}>{t("重试")}</Button></div> : !form ? <p role="status" className="text-xs text-gray-500">{t("加载中")}</p> : <>
      <p className="text-xs leading-5 text-gray-500">{t("留空自动发现。只影响此卡的下一次连接，不改认证身份，也不会立即重连。")}</p>
      <div><label htmlFor={`${id}-hosts`} className="mb-1 block text-xs text-gray-500">{t("优先 ePDG 地址（每行一个，不含端口）")}</label><Textarea id={`${id}-hosts`} rows={3} value={form.hosts} disabled={busy} onChange={e => setForm({ ...form, hosts: e.target.value })} /></div>
      <div><label htmlFor={`${id}-plmns`} className="mb-1 block text-xs text-gray-500">{t("补充发现 PLMN（MCC-MNC）")}</label><Textarea id={`${id}-plmns`} rows={2} value={form.plmns} disabled={busy} onChange={e => setForm({ ...form, plmns: e.target.value })} />{!!preview.length && <div className="mt-2 space-y-1 break-all font-mono text-[11px] text-gray-500">{preview.map(host => <div key={host}>{host}</div>)}</div>}</div>
      <div className="flex items-center justify-between gap-3 text-sm"><span>{t("仅使用手动地址")}</span><Switch checked={form.manualOnly} disabled={busy} ariaLabel={t("仅使用手动 ePDG 地址")} onChange={manualOnly => setForm({ ...form, manualOnly })} /></div>
      <div className="flex items-center justify-between gap-3"><div><div className="text-sm">{t("公共地域 DNS 回退")}</div><p className="mt-1 text-xs leading-5 text-gray-500">{t("默认关闭。开启后，仅当系统 DNS 失败时向 Google DNS 查询 ePDG 域名；不发送 SIM 身份。")}</p></div><Switch checked={form.geographicDnsFallback} disabled={busy} ariaLabel={t("公共地域 DNS 回退")} onChange={geographicDnsFallback => setForm({ ...form, geographicDnsFallback })} /></div>
      <div className="flex flex-wrap justify-end gap-2"><Button disabled={busy} onClick={() => setForm({ ...form, hosts: "", plmns: "", manualOnly: false, geographicDnsFallback: false })}>{t("恢复自动发现")}</Button><Button variant="primary" loading={busy} disabled={JSON.stringify(form) === saved} onClick={() => void save()}>{t("保存此卡 ePDG")}</Button></div>
    </>}
  </div>;
}
