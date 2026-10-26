import { useI18n } from "../../lib/i18n";
import { Switch } from "../ui/Switch";
import type { CarrierLineForm } from "./CarrierLinesForm";

export interface SMSStatistics { outgoingAccepted: number; outgoingUnknown: number; incomingQueued: number; incomingAccepted: number; incomingUnknown: number; incomingRejected: number; error?: string }
interface Props {
  enabled: boolean; busy: boolean; lines: CarrierLineForm[];
  accounts: { username: string; smsSendLines: string[] }[];
  peerEnabled?: boolean; peerIncomingLines?: string[];
  statistics?: SMSStatistics;
  onEnabled: (enabled: boolean) => void;
  onLines: (lines: CarrierLineForm[]) => void;
  onSendPermission: (username: string, line: string, allowed: boolean) => void;
}
export function SMSGatewayForm({ enabled, busy, lines, accounts, peerEnabled, peerIncomingLines = [], statistics, onEnabled, onLines, onSendPermission }: Props) {
  const { t } = useI18n();
  const configured = accounts.filter(account => /^\d{2,12}$/.test(account.username));
  const patch = (key: string, value: Partial<CarrierLineForm>) => onLines(lines.map(line => line.key === key ? { ...line, ...value } : line));
  return <section className="space-y-4 rounded-xl border border-gray-200 p-4 dark:border-white/10">
    <div className="flex items-center justify-between gap-4"><div><h4 className="text-sm font-semibold">{t("手机短信桥接")}</h4><p className="mt-1 text-xs leading-5 text-gray-500">{t("可选，默认关闭。与通话、内部分机消息权限独立。")}</p></div><Switch checked={enabled} disabled={busy} ariaLabel={t("启用手机短信桥接（保存后生效）")} onChange={onEnabled} /></div>
    {enabled && <>
      <p className="text-xs leading-5 text-amber-600 dark:text-amber-400">{t("分机发送手机短信可能产生费用；仅使用已注册 IMS，不改走 AT 或其他模组。")}</p>
      {!lines.some(line => line.deviceId) && <p className="text-xs text-gray-500">{t("请先在模组线路中添加模组，再分别授权收发。")}</p>}
      {lines.filter(line => line.deviceId).map(line => <div key={line.key} className="space-y-3 rounded-lg bg-gray-50 p-3 dark:bg-white/5">
        <h5 className="break-all text-sm font-medium">{line.label || line.deviceId}</h5>
        <div className="flex items-center justify-between gap-3 text-xs text-gray-500"><span>{t("允许此卡发送短信")}</span><Switch size="small" checked={line.allowSmsSend} disabled={busy} ariaLabel={`${t("允许此卡发送短信")} ${line.label || line.deviceId}`} onChange={allowSmsSend => patch(line.key, { allowSmsSend })} /></div>
        {line.allowSmsSend && <div className="space-y-3 border-l-2 border-sky-200 pl-3 dark:border-sky-500/30"><p className="text-xs text-gray-500">{t("可使用此卡发短信的分机")}</p>
          {configured.map(account => <div key={account.username} className="flex items-center justify-between gap-3 text-xs"><span>{account.username}</span><Switch size="small" disabled={busy} checked={account.smsSendLines.includes(line.deviceId)} ariaLabel={`${t("发短信授权")} ${account.username} ${line.label || line.deviceId}`} onChange={allowed => onSendPermission(account.username, line.deviceId, allowed)} /></div>)}
          {!configured.length && <p className="text-xs text-gray-500">{t(peerEnabled ? "本地分机未配置；中继目标在下方单独设置。" : "请先添加分机")}</p>}
        </div>}
        <div className="flex items-center justify-between gap-3 border-t border-gray-200 pt-3 text-xs text-gray-500 dark:border-white/10"><span>{t("将此卡收件转发到分机")}</span><Switch size="small" disabled={busy} checked={line.allowSmsReceive} ariaLabel={`${t("将此卡收件转发到分机")} ${line.label || line.deviceId}`} onChange={allowSmsReceive => patch(line.key, { allowSmsReceive })} /></div>
        {line.allowSmsReceive && <div className="space-y-3 border-l-2 border-emerald-200 pl-3 dark:border-emerald-500/30">
          {configured.map(account => <div key={account.username} className="flex items-center justify-between gap-3 text-xs"><span>{account.username}</span><Switch size="small" disabled={busy || !line.smsRecipients.includes(account.username) && line.smsRecipients.length >= 16} checked={line.smsRecipients.includes(account.username)} ariaLabel={`${t("接收短信转发")} ${account.username} ${line.label || line.deviceId}`} onChange={allowed => patch(line.key, { smsRecipients: allowed ? [...line.smsRecipients, account.username] : line.smsRecipients.filter(user => user !== account.username) })} /></div>)}
          {!line.smsRecipients.length && <p className="text-xs text-gray-500">{t(peerIncomingLines.includes(line.deviceId) ? "未选择本地分机；收件按下方中继目标转发。" : "未选择分机，不会转发短信。")}</p>}
        </div>}
      </div>)}
      <p className="text-xs leading-5 text-gray-500">{t("回复地址绑定收信模组和 SIM。离线未投递会等待注册；结果未知不会自动重复发送。")}</p>
      {statistics && <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-gray-500" role="status"><span>{t("外发已接受")} {statistics.outgoingAccepted || 0}</span><span>{t("等待分机上线")} {statistics.incomingQueued || 0}</span><span>{t("转发已接受")} {statistics.incomingAccepted || 0}</span>{!!(statistics.outgoingUnknown || statistics.incomingUnknown || statistics.incomingRejected) && <span className="text-amber-600">{t("未确认或拒绝")} {(statistics.outgoingUnknown || 0) + (statistics.incomingUnknown || 0) + (statistics.incomingRejected || 0)}</span>}</div>}
      {statistics?.error && <p role="alert" className="text-xs text-red-500">{t("短信转发状态无法保存，请检查存储。")}</p>}
    </>}
  </section>;
}
