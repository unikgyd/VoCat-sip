import { useI18n } from "../../lib/i18n";
import { Button, Input } from "../ui";
import { Switch } from "../ui/Switch";
import type { CarrierLineForm } from "./CarrierLinesForm";
import { emptySIPPeer, sipPeerStatus, updatePeerInbound, updatePeerOutbound, type SIPPeer, type SIPPeerRuntime } from "./sipGatewayModel";

interface Props { id: string; realm: string; peer: SIPPeer | null; runtimePeer?: SIPPeerRuntime; statusStale: boolean; busy: boolean; serviceRunning: boolean; serviceError?: string; lines: CarrierLineForm[]; onChange: (peer: SIPPeer | null) => void }
export function AsteriskPeerForm({ id, realm, peer, runtimePeer, statusStale, busy, serviceRunning, serviceError, lines, onChange }: Props) {
  const { t } = useI18n();
  const value = peer || emptySIPPeer();
  const patch = (change: Partial<SIPPeer>) => onChange({ ...value, ...change });
  const patchRoute = (deviceId: string, change: { callTarget?: string; smsTarget?: string }) => {
    const previous = value.incomingRoutes.find(route => route.deviceId === deviceId) || { deviceId, callTarget: "", smsTarget: "" };
    const next = { ...previous, ...change };
    patch({ incomingRoutes: [...value.incomingRoutes.filter(route => route.deviceId !== deviceId), ...(next.callTarget || next.smsTarget ? [next] : [])] });
  };
  const toggleLine = (key: "outgoingLines" | "smsSendLines", deviceId: string, allowed: boolean) => patch({ [key]: allowed ? [...new Set([...value[key], deviceId])] : value[key].filter(id => id !== deviceId) });
  const labelClass = "mb-1 block text-xs font-medium text-gray-500 dark:text-gray-400";
  const presentation = sipPeerStatus({ running: serviceRunning, error: serviceError, peer: runtimePeer }, statusStale);
  const tone = presentation.tone === "danger" ? "bg-red-500" : presentation.tone === "warning" ? "bg-amber-400" : "bg-gray-400";
  return <details className="rounded-xl border border-gray-200 p-4 dark:border-white/10">
    <summary className="cursor-pointer text-sm font-medium">{t("可选 Asterisk 中继")}<span className="ml-3 inline-flex items-center gap-2 text-xs font-normal text-gray-500"><span className={`h-2 w-2 rounded-full ${tone}`} aria-hidden="true" />{t(presentation.label)}</span></summary>
    <div className="mt-4 space-y-4">
      <div className="flex items-center justify-between gap-4"><p className="text-xs leading-5 text-gray-500">{t("固定可信内网端点，与本地分机共存；不发送 REGISTER，也不自动探测或启动 IMS。")}</p><Switch checked={value.enabled} disabled={busy} ariaLabel={t("启用 Asterisk 中继（保存后生效）")} onChange={enabled => patch({ enabled })} /></div>
      {peer && <>
        <div><label htmlFor={`${id}-peer-endpoint`} className={labelClass}>{t("固定中继端点（IP:端口）")}</label><Input id={`${id}-peer-endpoint`} disabled={busy} value={value.endpoint} autoComplete="off" onChange={event => patch({ endpoint: event.target.value.trim() })} /><p className="mt-1 text-xs leading-5 text-gray-500">{t("须位于允许接入的网段内，与监听地址同协议族；不填写域名或 SIP URI。")}</p></div>
        <div className="grid gap-4 md:grid-cols-2">
          <fieldset className="min-w-0 space-y-3 rounded-lg bg-gray-50 p-3 dark:bg-white/5"><legend className="px-1 text-xs font-medium">{t("入站认证 · Asterisk → 本机")}</legend>
            <p className="break-all text-xs text-gray-500">Realm: <span className="font-mono">{realm}</span></p>
            <div><label htmlFor={`${id}-peer-in-user`} className={labelClass}>{t("中继入站账号")}</label><Input id={`${id}-peer-in-user`} disabled={busy} value={value.inboundAuth.username} autoComplete="off" onChange={event => onChange(updatePeerInbound(value, { username: event.target.value }))} /></div>
            <div><label htmlFor={`${id}-peer-in-password`} className={labelClass}>{t("中继入站密码")}</label><Input id={`${id}-peer-in-password`} disabled={busy} type="password" autoComplete="new-password" value={value.inboundAuth.password || ""} placeholder={t(value.inboundAuth.hasPassword ? "留空保留已保存密码" : "至少 12 字节")} onChange={event => onChange(updatePeerInbound(value, { password: event.target.value }))} /></div>
            <p className="text-xs leading-5 text-gray-500">{t("使用本机 Realm。中继账号独立于数字分机；来源地址匹配也必须认证。")}</p>
          </fieldset>
          <fieldset className="min-w-0 space-y-3 rounded-lg bg-gray-50 p-3 dark:bg-white/5"><legend className="px-1 text-xs font-medium">{t("出站认证 · 本机 → Asterisk")}</legend>
            <div><label htmlFor={`${id}-peer-out-user`} className={labelClass}>{t("中继出站账号")}</label><Input id={`${id}-peer-out-user`} disabled={busy} value={value.outboundAuth.username} autoComplete="off" onChange={event => onChange(updatePeerOutbound(value, { username: event.target.value }))} /></div>
            <div><label htmlFor={`${id}-peer-out-realm`} className={labelClass}>{t("出站预期 Realm")}</label><Input id={`${id}-peer-out-realm`} disabled={busy} value={value.outboundAuth.realm} autoComplete="off" onChange={event => onChange(updatePeerOutbound(value, { realm: event.target.value }))} /></div>
            <div><label htmlFor={`${id}-peer-out-password`} className={labelClass}>{t("中继出站密码")}</label><Input id={`${id}-peer-out-password`} disabled={busy} type="password" autoComplete="new-password" value={value.outboundAuth.password || ""} placeholder={t(value.outboundAuth.hasPassword ? "留空保留已保存密码" : "至少 12 字节")} onChange={event => onChange(updatePeerOutbound(value, { password: event.target.value }))} /></div>
            <p className="text-xs leading-5 text-gray-500">{t("只响应固定端点和匹配 Realm 的挑战；修改账号或 Realm 须重填对应密码。")}</p>
            <p className="text-xs leading-5 text-gray-500">{t("任一通话方向都需要出站凭据，以处理模组先挂断时的 BYE 认证。")}</p>
            <div className="flex flex-wrap gap-x-5 gap-y-2 text-xs">{["SHA-256", "MD5"].map(algorithm => <label key={algorithm} className="flex items-center gap-2"><input type="checkbox" disabled={busy} checked={value.outboundAuth.algorithms.includes(algorithm)} onChange={event => onChange(updatePeerOutbound(value, { algorithms: event.target.checked ? [...value.outboundAuth.algorithms, algorithm] : value.outboundAuth.algorithms.filter(item => item !== algorithm) }))} />{algorithm}{algorithm === "MD5" && ` · ${t("旧客户端兼容")}`}</label>)}</div>
          </fieldset>
        </div>
        <div className="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-white/10"><h5 className="text-sm font-medium">{t("中继方向权限（默认关闭）")}</h5>
          {([["allowCallsFromPeer", "允许 Asterisk 使用模组外呼"], ["allowSmsFromPeer", "允许 Asterisk 发送手机短信"], ["allowCallsToPeer", "允许模组来电通知 Asterisk"], ["allowSmsToPeer", "允许模组短信转发到 Asterisk"]] as const).map(([key, label]) => <div key={key} className="flex items-center justify-between gap-3 text-xs text-gray-500"><span>{t(label)}</span><Switch size="small" checked={value[key]} disabled={busy} ariaLabel={t(label)} onChange={allowed => patch({ [key]: allowed })} /></div>)}
          <p className="text-xs leading-5 text-gray-500">{t("还须开启对应通话／短信及线路权限；默认线路不自动赋权。手机短信和外呼可能收费。")}</p>
        </div>
        <details><summary className="cursor-pointer text-xs font-medium text-gray-500">{t("逐线授权与入站目标")}</summary><div className="mt-3 space-y-3">
          {!lines.some(line => line.deviceId) && <p className="text-xs text-gray-500">{t("请先添加模组线路；中继不会自动选择其他 SIM。")}</p>}
          {lines.filter(line => line.deviceId).map(line => {
            const route = value.incomingRoutes.find(item => item.deviceId === line.deviceId);
            const name = line.label || line.deviceId;
            return <div key={line.key} className="space-y-3 rounded-lg bg-gray-50 p-3 dark:bg-white/5"><h6 className="break-all text-sm font-medium">{name}</h6>
              <div className="flex items-center justify-between gap-3 text-xs"><span>{t("中继可使用此线外呼")}{!line.allowOutgoing && ` · ${t("线路权限关闭")}`}</span><Switch size="small" checked={value.outgoingLines.includes(line.deviceId)} disabled={busy || !value.allowCallsFromPeer} ariaLabel={`${t("中继外呼授权")} ${name}`} onChange={allowed => toggleLine("outgoingLines", line.deviceId, allowed)} /></div>
              <div className="flex items-center justify-between gap-3 text-xs"><span>{t("中继可使用此线发短信")}{!line.allowSmsSend && ` · ${t("线路权限关闭")}`}</span><Switch size="small" checked={value.smsSendLines.includes(line.deviceId)} disabled={busy || !value.allowSmsFromPeer} ariaLabel={`${t("中继短信授权")} ${name}`} onChange={allowed => toggleLine("smsSendLines", line.deviceId, allowed)} /></div>
              <div className="grid gap-3 sm:grid-cols-2"><div><label htmlFor={`${id}-peer-${line.key}-call`} className={labelClass}>{t("来电在 Asterisk 的目标 user")}</label><Input id={`${id}-peer-${line.key}-call`} disabled={busy} value={route?.callTarget || ""} autoComplete="off" onChange={event => patchRoute(line.deviceId, { callTarget: event.target.value })} /></div><div><label htmlFor={`${id}-peer-${line.key}-sms`} className={labelClass}>{t("短信在 Asterisk 的目标 user")}</label><Input id={`${id}-peer-${line.key}-sms`} disabled={busy} value={route?.smsTarget || ""} autoComplete="off" onChange={event => patchRoute(line.deviceId, { smsTarget: event.target.value })} /></div></div>
              <p className="text-xs leading-5 text-gray-500">{t("目标留空不转发。请在模组线路中另行开启来电／收件权限；不改变本地分机目标。")}</p>
            </div>;
          })}
        </div></details>
        <div className="flex flex-wrap items-center justify-between gap-3"><p className="text-xs leading-5 text-gray-500">{t("保存成功不代表 Asterisk 连通或真实通话、短信已验收。")}</p><Button size="small" variant="text" disabled={busy} onClick={() => onChange(null)}>{t("移除中继配置（保存后生效）")}</Button></div>
      </>}
    </div>
  </details>;
}
