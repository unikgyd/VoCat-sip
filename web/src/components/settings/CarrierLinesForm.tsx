import { useEffect, useState } from "react";
import { AddRegular, DeleteRegular } from "@fluentui/react-icons";
import { api } from "../../api";
import { useI18n } from "../../lib/i18n";
import { Button, Input } from "../ui";
import { Switch } from "../ui/Switch";

export interface CarrierLine { deviceId: string; label: string; prefix: string; default: boolean; allowOutgoing: boolean; allowIncoming: boolean; incomingExtensions: string[]; countryCode: string; nationalNumberLength: number; allowSmsSend: boolean; allowSmsReceive: boolean; smsRecipients: string[] }
export interface CarrierLineForm extends CarrierLine { key: string }
interface Props { id: string; enabled: boolean; smsEnabled: boolean; callsEnabled: boolean; busy: boolean; lines: CarrierLineForm[]; accounts: { username: string; allowCalls: boolean }[]; peerEnabled?: boolean; peerIncomingLines?: string[]; onEnabled: (value: boolean) => void; onLines: (value: CarrierLineForm[]) => void; createKey: () => string }

export function CarrierLinesForm({ id, enabled, smsEnabled, callsEnabled, busy, lines, accounts, peerEnabled, peerIncomingLines = [], onEnabled, onLines, createKey }: Props) {
  const { t } = useI18n();
  const [devices, setDevices] = useState<{ id: string; name?: string }[]>([]);
  const [devicesFailed, setDevicesFailed] = useState(false);
  const [revision, setRevision] = useState(0);
  const visible = enabled || smsEnabled || lines.length > 0;
  useEffect(() => {
    if (!visible) return;
    let active = true;
    api<{ devices: { id: string; name?: string }[] }>("/devices", { signal: AbortSignal.timeout(10000) }).then(value => {
      if (active) { setDevices(value.devices); setDevicesFailed(false); }
    }).catch(() => { if (active) setDevicesFailed(true); });
    return () => { active = false; };
  }, [visible, revision]);
  const patch = (key: string, change: Partial<CarrierLine>) => onLines(lines.map(line => line.key === key ? { ...line, ...change } : line));
  const labelClass = "mb-1 block text-xs font-medium text-gray-500 dark:text-gray-400";
  const selectClass = "h-8 w-full min-w-0 rounded-lg border border-gray-300 bg-white px-3 text-sm dark:border-white/15 dark:bg-gray-900";
  return <div className="space-y-4 rounded-xl border border-gray-200 p-4 dark:border-white/10">
    <div className="flex items-center justify-between gap-4">
      <div><h4 className="text-sm font-semibold">{t("模组通话桥接")}</h4><p className="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">{t("默认关闭。仅使用已注册 IMS，不自动启动线路或切换模组。")}</p></div>
      <Switch checked={enabled} disabled={busy} ariaLabel={t("启用模组通话桥接（保存后生效）")} onChange={onEnabled} />
    </div>
    {visible && <>
      <p className="text-xs leading-5 text-gray-500 dark:text-gray-400">{t("通话与短信共用选线规则，但授权分别设置。")}</p>
      {enabled && !callsEnabled && <p className="text-xs text-amber-600 dark:text-amber-400">{t("请同时开启分机通话，外呼权限才会生效。")}</p>}
      <div className="flex flex-wrap items-center justify-between gap-2"><h5 className="text-sm font-medium">{t("模组线路")}</h5>
        <Button size="small" icon={<AddRegular />} disabled={busy || lines.length >= 32} onClick={() => onLines([...lines, { key: createKey(), deviceId: "", label: "", prefix: "", default: false, allowOutgoing: false, allowIncoming: false, incomingExtensions: [], countryCode: "", nationalNumberLength: 0, allowSmsSend: false, allowSmsReceive: false, smsRecipients: [] }])}>{t("添加线路")}</Button>
      </div>
      {devicesFailed && <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-amber-600"><span>{t("模组列表暂不可用，已有选线保持不变。")}</span><Button size="small" disabled={busy} onClick={() => setRevision(value => value + 1)}>{t("重试")}</Button></div>}
      {!lines.length && <p className="rounded-lg border border-dashed border-gray-200 p-4 text-xs text-gray-500 dark:border-white/10">{t("尚无线路，不预置模组、国家码或选号前缀。")}</p>}
      {lines.map(line => <div key={line.key} className="space-y-3 rounded-lg bg-gray-50 p-3 dark:bg-white/5">
        <div className="flex items-center justify-between gap-3"><span className="min-w-0 truncate text-sm font-medium">{line.label || line.deviceId || t("新线路")}</span><Button size="small" variant="text" icon={<DeleteRegular />} disabled={busy} aria-label={`${t("删除线路")} ${line.label || line.deviceId}`} onClick={() => onLines(lines.filter(item => item.key !== line.key))}>{t("删除")}</Button></div>
        <div className="grid min-w-0 gap-3 sm:grid-cols-3">
          <div className="min-w-0"><label htmlFor={`${id}-${line.key}-device`} className={labelClass}>{t("模组")}</label><select id={`${id}-${line.key}-device`} className={selectClass} disabled={busy} value={line.deviceId} onChange={e => patch(line.key, { deviceId: e.target.value })}>
            <option value="">{t("请选择模组")}</option>
            {line.deviceId && !devices.some(device => device.id === line.deviceId) && <option value={line.deviceId}>{line.deviceId} · {t("未在当前列表中")}</option>}
            {devices.map(device => <option key={device.id} value={device.id}>{device.name || device.id} ({device.id})</option>)}
          </select></div>
          <div><label htmlFor={`${id}-${line.key}-label`} className={labelClass}>{t("线路名称（可选）")}</label><Input id={`${id}-${line.key}-label`} disabled={busy} value={line.label} maxLength={80} onChange={e => patch(line.key, { label: e.target.value })} /></div>
          <div><label htmlFor={`${id}-${line.key}-prefix`} className={labelClass}>{t("指定选线前缀（可选）")}</label><Input id={`${id}-${line.key}-prefix`} disabled={busy} value={line.prefix} inputMode="numeric" maxLength={8} onChange={e => patch(line.key, { prefix: e.target.value })} /></div>
        </div>
        <details><summary className="cursor-pointer text-xs font-medium text-gray-500">{t("国内号码补全（可选）")}</summary><div className="mt-3 grid gap-3 sm:grid-cols-2">
          <div><label htmlFor={`${id}-${line.key}-country`} className={labelClass}>{t("国家码（不含 +）")}</label><Input id={`${id}-${line.key}-country`} disabled={busy} value={line.countryCode} inputMode="numeric" maxLength={3} onChange={e => patch(line.key, { countryCode: e.target.value })} /></div>
          <div><label htmlFor={`${id}-${line.key}-length`} className={labelClass}>{t("国内号码位数")}</label><Input id={`${id}-${line.key}-length`} disabled={busy} value={line.nationalNumberLength || ""} type="number" min={6} max={14} onChange={e => patch(line.key, { nationalNumberLength: Number(e.target.value) || 0 })} /></div>
          <p className="text-xs leading-5 text-gray-500 sm:col-span-2">{t("仅匹配指定长度才补国家码；留空时必须拨 + 开头的完整号码。")}</p>
        </div></details>
        <div className="flex items-center justify-between gap-3 text-xs text-gray-500"><span>{t("允许该线路外呼")}</span><Switch size="small" checked={line.allowOutgoing} disabled={busy} ariaLabel={`${t("允许该线路外呼")} ${line.label || line.deviceId}`} onChange={allowOutgoing => patch(line.key, { allowOutgoing })} /></div>
        <div className="border-t border-gray-200 pt-3 dark:border-white/10">
          <div className="flex items-center justify-between gap-3 text-xs text-gray-500"><span>{t("来电通知分机")}</span><Switch size="small" checked={line.allowIncoming} disabled={busy} ariaLabel={`${t("来电通知分机")} ${line.label || line.deviceId}`} onChange={allowIncoming => patch(line.key, { allowIncoming })} /></div>
          {line.allowIncoming && <div className="mt-3 space-y-3">
              <p className="text-xs leading-5 text-gray-500">{t("选定分机与在线网页共同提醒，先接者通话。网页语音需要 HTTPS，拒接会结束整通来电。")}</p>
            {!accounts.some(a => /^\d{2,12}$/.test(a.username)) && <p className="text-xs text-amber-600">{t(peerEnabled ? "本地分机未配置；中继目标在下方单独设置。" : "请先添加分机，并开启其通话权限。")}</p>}
            {accounts.filter(a => /^\d{2,12}$/.test(a.username)).map(account => <div key={account.username} className="flex items-center justify-between gap-3 text-xs text-gray-500"><span>{account.username}{!account.allowCalls && ` · ${t("通话权限关闭")}`}</span><Switch size="small" checked={line.incomingExtensions.includes(account.username)} disabled={busy || !account.allowCalls || !line.incomingExtensions.includes(account.username) && line.incomingExtensions.length >= 16} ariaLabel={`${t("接收线路来电")} ${line.label || line.deviceId} ${account.username}`} onChange={allowed => patch(line.key, { incomingExtensions: allowed ? [...line.incomingExtensions, account.username] : line.incomingExtensions.filter(user => user !== account.username) })} /></div>)}
            {!line.incomingExtensions.length && <p className="text-xs text-amber-600">{t(peerIncomingLines.includes(line.deviceId) ? "未选择本地分机；此线来电按下方中继目标转发，网页仍可参与接听。" : "未选择分机，不转接来电；保留原通知与接听接口。")}</p>}
          </div>}
        </div>
      </div>)}
      {!!lines.length && <div><label htmlFor={`${id}-default-line`} className={labelClass}>{t("默认出站线路")}</label><select id={`${id}-default-line`} className={selectClass} disabled={busy} value={lines.find(line => line.default)?.key || ""} onChange={e => onLines(lines.map(line => ({ ...line, default: line.key === e.target.value })))}>
        <option value="">{t("无默认线路（须指定选线前缀）")}</option>{lines.map(line => <option key={line.key} value={line.key}>{line.label || line.deviceId || t("新线路")}</option>)}
      </select></div>}
      <p className="text-xs leading-5 text-gray-500">{t("已知分机号始终优先；指定线路不可用时，不会自动换卡重拨。")}</p>
    </>}
  </div>;
}
