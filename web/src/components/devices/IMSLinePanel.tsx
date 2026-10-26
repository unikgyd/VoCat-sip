import { useEffect, useId, useState } from "react";
import { api, apiMessage } from "../../api";
import { useI18n } from "../../lib/i18n";
import { Button, message, StatusDot } from "../ui";
import { Switch } from "../ui/Switch";
import type { IMSMode, TelephonyPolicy } from "../../types";
import type { DeviceDetail } from "./types";
import { hostIMSStatus } from "./imsLineModel";
import { voWiFiStatus } from "./voWifiStatusModel";

interface PolicyResponse { policy: TelephonyPolicy | null; legacy: boolean }

export function IMSLinePanel({ device, onUpdated }: { device: DeviceDetail; onUpdated: () => void | Promise<void> }) {
  const { t } = useI18n();
  const id = useId();
  const [saved, setSaved] = useState<TelephonyPolicy | null>(null);
  const [draft, setDraft] = useState<TelephonyPolicy | null>(null);
  const [busy, setBusy] = useState(false);
  const [loadError, setLoadError] = useState("");
  const [reload, setReload] = useState(0);
  const currentPolicy = JSON.stringify(device.telephonyPolicy ?? null);
  useEffect(() => {
    let stale = false;
    setSaved(null); setDraft(null); setLoadError("");
    api<PolicyResponse>(`/devices/${encodeURIComponent(device.id)}/telephony`).then(result => {
      if (!stale) { setSaved(result.policy); setDraft(result.policy); }
    }).catch(err => { if (!stale) setLoadError(apiMessage(err) || t("线路设置加载失败")); });
    return () => { stale = true; };
  }, [device.id, device.modem?.iccid, currentPolicy, reload]);
  const changed = JSON.stringify(draft) !== JSON.stringify(saved) || !device.telephonyPolicy;
  const reader = device.deviceType === "usb_sim_reader";
  const wifi = voWiFiStatus(device);
  const host = hostIMSStatus(device);
  const update = (patch: Partial<TelephonyPolicy>) => setDraft(value => value ? { ...value, ...patch } : value);
  const save = async () => {
    if (!draft) return;
    setBusy(true);
    try {
      const result = await api<PolicyResponse>(`/devices/${encodeURIComponent(device.id)}/telephony`, {
        method: "PATCH", body: draft, signal: AbortSignal.timeout(60000),
      });
      setSaved(result.policy); setDraft(result.policy);
      message.success(t("线路设置已保存"));
      await onUpdated();
    } catch (err) { message.error(apiMessage(err) || t("线路设置保存失败")); }
    finally { setBusy(false); }
  };
  const reconnect = async (access: IMSMode) => {
    setBusy(true);
    try {
      await api(`/devices/${encodeURIComponent(device.id)}/telephony/actions/reconnect`, {
        method: "POST", body: { access }, signal: AbortSignal.timeout(25000),
      });
      message.success(t("已请求重新连接")); await onUpdated();
    } catch (err) { message.error(apiMessage(err) || t("重连失败")); }
    finally { setBusy(false); }
  };
  const selectClass = "w-full rounded-lg border border-gray-200 bg-transparent px-3 py-2 text-sm dark:border-white/15";
  return <section className="ui-panel-muted p-4" aria-labelledby={`${id}-title`} aria-busy={busy}>
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div><h3 id={`${id}-title`} className="text-sm font-semibold">{t("电话线路")}</h3>
        <p className="mt-1 text-xs text-gray-500">{t("蜂窝与 Wi-Fi 独立连接，设置随 SIM 卡保存。默认线路只影响新发起的业务。")}</p></div>
      <Button loading={busy} disabled={!draft || !changed} onClick={() => void save()}>{t("应用")}</Button>
    </div>
    {loadError ? <div className="mt-3 text-sm text-red-600" role="alert">{loadError} <Button variant="text" onClick={() => setReload(value => value + 1)}>{t("重试")}</Button></div> : !draft ? <p className="mt-3 text-xs text-gray-500">{t("等待识别 SIM 卡及线路设置")}</p> : <>
      <div className="mt-4 grid gap-4 sm:grid-cols-2">
        <div className="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-white/10">
          <div className="flex items-center justify-between gap-2"><span className="text-sm font-semibold">{t("蜂窝连接")}</span>
            <Switch checked={draft.autoConnectCellular && !reader} disabled={busy || reader} ariaLabel={t("自动连接蜂窝线路")}
              onChange={value => update({ autoConnectCellular: value, ...(value ? { wifiRadioPolicy: "coexist" as const } : {}) })} /></div>
          <label className="block text-xs text-gray-500" htmlFor={`${id}-cell`}>{t("蜂窝语音方式")}</label>
          <select id={`${id}-cell`} className={selectClass} value={draft.cellularMode} disabled={busy || reader} onChange={e => update({ cellularMode: e.target.value as TelephonyPolicy["cellularMode"] })}>
            <option value="module">{t("模块语音")}</option><option value="host_ims">Host IMS</option>
          </select>
          <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-gray-500" role="status">
            <span>{reader ? t("此读卡器不支持蜂窝连接") : saved?.cellularMode === "host_ims" ? `Host IMS · ${t(host.label)}` : t(device.radioRegistered ? "蜂窝已注册" : "蜂窝未注册")}</span>
            {!reader && saved?.autoConnectCellular && device.telephonyPolicy && <Button size="small" variant="text" disabled={busy || changed} onClick={() => void reconnect(saved.cellularMode)}>{t("重连")}</Button>}
          </div>
        </div>
        <div className="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-white/10">
          <div className="flex items-center justify-between gap-2"><span className="text-sm font-semibold">Wi-Fi Calling</span>
            <Switch checked={draft.autoConnectWifi} disabled={busy} ariaLabel={t("自动连接 Wi-Fi 通话")} onChange={value => update({ autoConnectWifi: value })} /></div>
          <label className="block text-xs text-gray-500" htmlFor={`${id}-radio`}>{t("蜂窝射频策略")}</label>
          <select id={`${id}-radio`} className={selectClass} value={draft.wifiRadioPolicy} disabled={busy} onChange={e => update({ wifiRadioPolicy: e.target.value as TelephonyPolicy["wifiRadioPolicy"] })}>
            <option value="coexist">{t("保留蜂窝连接")}</option><option value="exclusive" disabled={draft.autoConnectCellular}>{t("Wi-Fi 独占，关闭蜂窝射频")}</option>
          </select>
          <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-gray-500" role="status"><span>{t(wifi.label)}</span>
            {saved?.autoConnectWifi && device.telephonyPolicy && <Button size="small" variant="text" disabled={busy || changed} onClick={() => void reconnect("vowifi")}>{t("重新检查")}</Button>}
          </div>
        </div>
      </div>
      <div className="mt-4"><label htmlFor={`${id}-route`} className="mb-1 block text-xs text-gray-500">{t("默认呼出与短信线路")}</label>
        <select id={`${id}-route`} className={selectClass} value={draft.defaultRoute} disabled={busy} onChange={e => update({ defaultRoute: e.target.value as IMSMode })}>
          <option value="module" disabled={reader}>{t("模块语音")}</option><option value="host_ims" disabled={reader}>Host IMS</option><option value="vowifi">Wi-Fi Calling</option>
        </select>
        <p className="mt-2 text-xs text-gray-500">{t("默认线路未就绪时会提示失败；已有通话和交互会话继续使用原线路。")}</p>
      </div>
    </>}
  </section>;
}

export function OverviewHostIMSCard({ device }: { device: DeviceDetail }) {
  const { t } = useI18n();
  const status = hostIMSStatus(device);
  const rt = device.cellularImsRuntime ?? device.imsRuntime;
  return <div className="space-y-3">
    <div className="flex items-center gap-2.5" role="status"><StatusDot tone={status.tone} size="sm" animated={status.animated} /><span className="text-sm font-semibold">Host IMS · {t(status.label)}</span></div>
    <div className="flex flex-wrap gap-x-4 gap-y-2 text-xs text-gray-500">
      {[{ label: "SIM", ready: rt?.simReady }, { label: t("蜂窝承载"), ready: rt?.accessReady }, { label: "IMS", ready: rt?.imsReady }, { label: "SMS", ready: rt?.smsReady }].map(item => <span key={item.label} className="flex items-center gap-1.5"><StatusDot tone={item.ready ? "success" : !(device.telephonyPolicy?.autoConnectCellular ?? device.imsEnabled) ? "neutral" : rt?.phase === "failed" ? "danger" : "warning"} size="sm" />{item.label}</span>)}
    </div>
    <details className="rounded-lg border border-gray-200 px-3 py-2 text-xs dark:border-white/10"><summary className="cursor-pointer text-gray-500">{t("详情")}</summary><dl className="mt-2 space-y-2 break-words">
      <div><dt className="text-gray-500">{t("数据平面")}</dt><dd className="font-mono">{rt?.dataplaneMode || "cellular-host-ims"}</dd></div>
      <div><dt className="text-gray-500">{t("当前阶段")}</dt><dd>{rt?.phase || "idle"}</dd></div>
      {rt?.lastErrorClass && <div><dt className="text-gray-500">{t("错误分类")}</dt><dd className="font-mono">{rt.lastErrorClass}</dd></div>}
    </dl></details>
  </div>;
}
