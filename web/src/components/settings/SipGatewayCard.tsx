import { useCallback, useEffect, useId, useRef, useState } from "react";
import { AddRegular, ArrowClockwiseRegular, CallRegular, DeleteRegular } from "@fluentui/react-icons";
import { api, apiMessage } from "../../api";
import { useI18n } from "../../lib/i18n";
import { Button, Input, Textarea, message } from "../ui";
import { Switch } from "../ui/Switch";
import { CardDecor, CardIcon, CardTitle } from "./Cards";
import { CarrierLinesForm, type CarrierLineForm } from "./CarrierLinesForm";
import { SMSGatewayForm, type SMSStatistics } from "./SMSGatewayForm";
import { AsteriskPeerForm } from "./AsteriskPeerForm";
import { adoptSIPConfig, restrictPeerLines, serializeSIPForm, sipPeerFormError, updateSIPAccount, updateSIPRealm, type SIPAccount as Account, type SIPConfig as Config, type SIPForm as Form, type SIPPeerRuntime } from "./sipGatewayModel";

interface Status { running: boolean; listen: string; error?: string; peer?: SIPPeerRuntime; bindings: { username: string; source: string; expiresAt: string }[]; calls?: { id: string; caller: string; callee: string; phase: string; deviceId?: string }[]; capabilities: { messages: boolean; calls: boolean; carrierBridge: boolean; carrierOutgoing?: boolean }; smsStatistics?: SMSStatistics }
interface View { config: Config; status: Status }

export function SipGatewayCard() {
  const { t } = useI18n();
  const id = useId();
  const serial = useRef(0);
  const [form, setForm] = useState<Form | null>(null);
  const [saved, setSaved] = useState("");
  const [status, setStatus] = useState<Status | null>(null);
  const [statusStale, setStatusStale] = useState(false);
  const [loadError, setLoadError] = useState(false);
  const [busy, setBusy] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const mounted = useRef(true);

  const adopt = useCallback((view: View) => {
    const next = adoptSIPConfig(view.config, () => `${++serial.current}`);
    setForm(next); setSaved(JSON.stringify(next)); setStatus(view.status); setLoadError(false); setStatusStale(false);
  }, []);
  useEffect(() => {
    let active = true;
    mounted.current = true;
    api<View>("/settings/sip", { signal: AbortSignal.timeout(10000) }).then(data => { if (active) adopt(data); })
      .catch(() => { if (active) setLoadError(true); });
    return () => { active = false; mounted.current = false; };
  }, [adopt]);

  const refresh = useCallback(async () => {
    setRefreshing(true);
    try {
      const view = await api<View>("/settings/sip", { signal: AbortSignal.timeout(10000) });
      if (!mounted.current) return;
      if (!form || JSON.stringify(form) === saved) adopt(view); else setStatus(view.status); // never erase unsaved edits
      setLoadError(false); setStatusStale(false);
    } catch { if (mounted.current) { setStatusStale(true); message.error(t("SIP 状态刷新失败")); } }
    finally { if (mounted.current) setRefreshing(false); }
  }, [adopt, form, saved, t]);

  // Refresh status without fetching raw SIP traces or modifying unsaved configuration edits.
  useEffect(() => {
    if (busy) return;
    let active = true;
    const timer = window.setInterval(() => {
      api<View>("/settings/sip", { signal: AbortSignal.timeout(10000) }).then(view => {
        if (!active) return;
        if (!form || JSON.stringify(form) === saved) adopt(view);
        else setStatus(view.status);
        setStatusStale(false);
      }).catch(() => { if (active) setStatusStale(true); });
    }, 15000);
    return () => { active = false; window.clearInterval(timer); };
  }, [adopt, busy, form, saved]);

  const patch = (value: Partial<Form>) => setForm(prev => prev ? { ...prev, ...value } : prev);
  const patchLines = (carrierLines: CarrierLineForm[]) => setForm(prev => {
    if (!prev) return prev;
    const known = new Set(carrierLines.map(line => line.deviceId));
    return { ...prev, carrierLines, peer: restrictPeerLines(prev.peer, known), accounts: prev.accounts.map(account => ({ ...account, outgoingLines: account.outgoingLines.filter(id => known.has(id)), smsSendLines: account.smsSendLines.filter(id => known.has(id)) })) };
  });
  const patchAccount = (key: string, value: Partial<Account>) => setForm(prev => {
    if (!prev) return prev;
    const oldName = prev.accounts.find(a => a.key === key)?.username;
    return { ...prev, accounts: prev.accounts.map(a => a.key === key ? updateSIPAccount(a, value) : a), carrierLines: value.username !== undefined && value.username !== oldName ? prev.carrierLines.map(line => ({ ...line, incomingExtensions: line.incomingExtensions.filter(user => user !== oldName), smsRecipients: line.smsRecipients.filter(user => user !== oldName) })) : prev.carrierLines };
  });
  const save = async () => {
    if (!form) return;
    const names = new Set<string>();
    for (const account of form.accounts) {
      if (!/^\d{2,12}$/.test(account.username) || names.has(account.username)) { message.error(t("分机号须为 2–12 位数字，且不能重复")); return; }
      names.add(account.username);
      const size = new TextEncoder().encode(account.password || "").length;
      if ((!account.hasPassword || size > 0) && (size < 12 || size > 128)) { message.error(t("新密码须为 12–128 字节")); return; }
    }
    if (form.enabled && !form.accounts.length && !form.peer?.enabled) { message.error(t("请先添加分机或配置启用的中继，再启用服务。")); return; }
    const peerError = sipPeerFormError(form.peer);
    if (peerError) { message.error(t(peerError)); return; }
    const lineIDs = new Set<string>();
    for (const line of form.carrierLines) {
      if (!line.deviceId || lineIDs.has(line.deviceId) || (line.prefix && !/^\d{1,8}$/.test(line.prefix)) ||
        (line.countryCode ? !/^[1-9]\d{0,2}$/.test(line.countryCode) || line.nationalNumberLength < 6 || line.nationalNumberLength > 15 - line.countryCode.length : line.nationalNumberLength !== 0)) {
        message.error(t("请检查线路模组、前缀和号码补全设置；每个模组只能配置一次。")); return;
      }
      lineIDs.add(line.deviceId);
    }
    if (form.carrierEnabled && !form.carrierLines.length) { message.error(t("请先添加外呼线路。")); return; }
    setBusy(true);
    try {
      const view = await api<View>("/settings/sip", { method: "PUT", signal: AbortSignal.timeout(10000), body: serializeSIPForm(form) });
      if (mounted.current) { adopt(view); message.success(t("SIP 配置已保存")); }
    } catch (error) { if (mounted.current) message.error(apiMessage(error) || t("SIP 配置保存失败")); }
    finally { if (mounted.current) setBusy(false); }
  };
  const changed = form !== null && JSON.stringify(form) !== saved;
  const stateText = !status ? t(loadError ? "加载失败" : "加载中") : statusStale ? t("状态暂不可用") : status.error ? t("启动失败") : status.running ? t("分机服务运行中") : t("已关闭");
  const dot = !status ? (loadError ? "bg-red-500" : "bg-amber-400") : statusStale ? "bg-amber-400" : status.error ? "bg-red-500" : status.running ? "bg-emerald-500" : "bg-gray-400";
  const labelClass = "mb-1 block text-xs font-medium text-gray-500 dark:text-gray-400";
  const online = new Set(status?.bindings.map(b => b.username) || []);

  return <section className="ui-card group relative overflow-hidden p-5 sm:p-8" aria-labelledby={`${id}-title`}>
    <CardDecor />
    <div className="relative z-10 mb-5 flex flex-wrap items-center justify-between gap-4">
      <div className="flex items-center gap-3">
        <CardIcon><CallRegular className="text-[24px]" /></CardIcon>
        <div id={`${id}-title`}><CardTitle title={t("SIP 分机服务")} subtitle={t("可选 · 实验阶段，分机通话与消息")} /></div>
      </div>
      <Switch ariaLabel={t("启用 SIP 分机服务（保存后生效）")} checked={form?.enabled || false} disabled={!form || busy} loading={busy} onChange={enabled => patch({ enabled })} />
    </div>
    <div className="relative z-10 space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl bg-gray-50 px-4 py-3 dark:bg-white/5">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm" role="status" aria-live="polite">
          <span className="flex items-center gap-2"><span className={`h-2 w-2 rounded-full ${dot}`} aria-hidden="true" />{stateText}</span>
          {status?.running && !statusStale && <span className="text-xs text-gray-500">UDP {status.listen} · {t("在线分机")} {online.size}</span>}
        </div>
        <Button size="small" icon={<ArrowClockwiseRegular />} loading={refreshing} disabled={busy} onClick={() => void refresh()}>{t("刷新状态")}</Button>
      </div>
      {loadError && !form && <p role="alert" className="text-sm text-red-500">{t("SIP 配置加载失败，请重试")}</p>}
      {status?.error && <p role="alert" className="break-words text-sm text-red-500">{status.error}</p>}
      {!statusStale && !!status?.calls?.length && <div className="space-y-2 rounded-xl bg-gray-50 p-4 dark:bg-white/5">
        {status.calls.map(call => <div key={call.id} className="flex flex-wrap items-center justify-between gap-2 text-sm"><span className="break-all">{call.caller} → {call.callee}{call.deviceId && <span className="ml-2 text-xs text-gray-500">· {call.deviceId}</span>}</span><span className="flex items-center gap-2 text-xs text-gray-500"><span className={`h-2 w-2 rounded-full ${call.phase === "active" ? "bg-emerald-500" : "bg-amber-400"}`} />{t(call.phase === "active" ? "通话中" : call.phase === "ringing" ? "振铃中" : call.phase === "dialing" ? "正在呼叫" : call.phase === "early_media" ? "呼叫进展" : "等待接听确认")}</span></div>)}
        <p className="text-xs leading-5 text-amber-600 dark:text-amber-400">{t("通话中请先挂断再保存；关闭服务将结束通话。")}</p>
      </div>}
      {form && <>
        <p className="text-xs leading-5 text-gray-500 dark:text-gray-400">{t("通话、分机消息、手机短信和可选中继分别授权。")}</p>
        <div className="max-w-xl"><label htmlFor={`${id}-listen`} className={labelClass}>{t("监听地址（IP:端口）")}</label>
          <Input id={`${id}-listen`} value={form.listen} disabled={busy} placeholder="0.0.0.0:5060" onChange={e => patch({ listen: e.target.value })} />
        </div>
        <div className="flex items-center justify-between gap-4 rounded-xl border border-gray-200 p-4 dark:border-white/10">
          <div><h4 className="text-sm font-semibold">{t("分机通话")}</h4><p className="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">{t("默认关闭。启用分机通话；模组外呼还须线路与账号授权。")}</p></div>
          <Switch checked={form.callsEnabled} disabled={busy} ariaLabel={t("启用分机通话（保存后生效）")} onChange={callsEnabled => patch({ callsEnabled })} />
        </div>
        <div className="flex items-center justify-between gap-4 rounded-xl border border-gray-200 p-4 dark:border-white/10">
          <div><h4 className="text-sm font-semibold">{t("分机文本消息")}</h4><p className="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">{t("默认关闭。仅在获准分机之间收发，不经过 SIM，也不会发送手机短信。")}</p></div>
          <Switch checked={form.messagesEnabled} disabled={busy} ariaLabel={t("启用分机文本消息（保存后生效）")} onChange={messagesEnabled => patch({ messagesEnabled })} />
        </div>
        <CarrierLinesForm id={id} enabled={form.carrierEnabled} smsEnabled={form.smsEnabled} callsEnabled={form.callsEnabled} busy={busy} lines={form.carrierLines} accounts={form.accounts} peerEnabled={form.peer?.enabled} peerIncomingLines={form.peer?.enabled && form.peer.allowCallsToPeer ? form.peer.incomingRoutes.filter(route => route.callTarget).map(route => route.deviceId) : []} createKey={() => `${++serial.current}`} onEnabled={carrierEnabled => patch({ carrierEnabled })} onLines={patchLines} />
        <SMSGatewayForm enabled={form.smsEnabled} busy={busy} lines={form.carrierLines} accounts={form.accounts} peerEnabled={form.peer?.enabled} peerIncomingLines={form.peer?.enabled && form.peer.allowSmsToPeer ? form.peer.incomingRoutes.filter(route => route.smsTarget).map(route => route.deviceId) : []} statistics={statusStale ? undefined : status?.smsStatistics} onEnabled={smsEnabled => patch({ smsEnabled })} onLines={patchLines} onSendPermission={(username, line, allowed) => { const account = form.accounts.find(account => account.username === username); if (account) patchAccount(account.key, { smsSendLines: allowed ? [...account.smsSendLines, line] : account.smsSendLines.filter(id => id !== line) }); }} />
        <AsteriskPeerForm id={id} realm={form.realm} peer={form.peer} runtimePeer={status?.peer} statusStale={statusStale} busy={busy} serviceRunning={status?.running === true} serviceError={status?.error} lines={form.carrierLines} onChange={peer => patch({ peer })} />
        <div className="space-y-3">
          <div className="flex items-center justify-between gap-3"><h4 className="text-sm font-semibold">{t("分机账号")}</h4>
            <Button size="small" icon={<AddRegular />} disabled={busy || form.accounts.length >= 64} onClick={() => patch({ accounts: [...form.accounts, { key: `${++serial.current}`, username: "", displayName: "", password: "", hasPassword: false, allowMessages: false, allowCalls: false, outgoingLines: [], smsSendLines: [] }] })}>{t("添加分机")}</Button>
          </div>
          {!form.accounts.length && <div className="rounded-xl border border-dashed border-gray-200 px-4 py-6 text-center text-sm text-gray-500 dark:border-white/10">{t("尚未添加分机。不预置账号或密码。")}</div>}
          {form.accounts.map(account => <div key={account.key} className="rounded-xl border border-gray-200 p-4 dark:border-white/10">
            <div className="mb-3 flex items-center justify-between gap-3">
              <span className="flex items-center gap-2 text-xs text-gray-500"><span className={`h-1.5 w-1.5 rounded-full ${online.has(account.username) ? "bg-emerald-500" : "bg-gray-400"}`} />{t(online.has(account.username) ? "已注册" : "未注册")}</span>
              <Button size="small" variant="text" icon={<DeleteRegular />} disabled={busy} aria-label={`${t("删除分机")} ${account.username}`} onClick={() => patch({ accounts: form.accounts.filter(a => a.key !== account.key), carrierLines: form.carrierLines.map(line => ({ ...line, incomingExtensions: line.incomingExtensions.filter(user => user !== account.username), smsRecipients: line.smsRecipients.filter(user => user !== account.username) })) })}>{t("删除")}</Button>
            </div>
            <div className="grid gap-3 md:grid-cols-3">
              <div><label htmlFor={`${id}-${account.key}-user`} className={labelClass}>{t("分机号")}</label><Input id={`${id}-${account.key}-user`} autoComplete="off" inputMode="numeric" maxLength={12} value={account.username} disabled={busy} onChange={e => patchAccount(account.key, { username: e.target.value })} /></div>
              <div><label htmlFor={`${id}-${account.key}-name`} className={labelClass}>{t("显示名称（可选）")}</label><Input id={`${id}-${account.key}-name`} maxLength={80} value={account.displayName} disabled={busy} onChange={e => patchAccount(account.key, { displayName: e.target.value })} /></div>
              <div><label htmlFor={`${id}-${account.key}-pass`} className={labelClass}>{t("分机密码")}</label><Input id={`${id}-${account.key}-pass`} type="password" autoComplete="new-password" value={account.password || ""} placeholder={t(account.hasPassword ? "留空保留已保存密码" : "至少 12 字节")} disabled={busy} onChange={e => patchAccount(account.key, { password: e.target.value })} /></div>
            </div>
            {account.hasPassword && <details className="mt-3"><summary className="cursor-pointer text-xs font-medium text-gray-500">{t("短信接收身份")}{account.resetCredentialIdentity && <span className="ml-2 text-amber-600 dark:text-amber-400">{t("保存后重置")}</span>}</summary><div className="mt-2 flex flex-wrap items-center gap-3"><p className="min-w-0 flex-1 text-xs leading-5 text-gray-500 dark:text-gray-400">{t("重置不改密码；旧待转发短信不再投递给该分机。重新填写密码（即使相同）也会重置。")}</p><Button size="small" variant="text" disabled={busy} aria-pressed={account.resetCredentialIdentity === true} onClick={() => patchAccount(account.key, { resetCredentialIdentity: !account.resetCredentialIdentity })}>{t(account.resetCredentialIdentity ? "撤销身份重置" : "重置接收身份")}</Button></div></details>}
            <div className="mt-3 flex items-center justify-between gap-3 text-xs text-gray-500 dark:text-gray-400"><span>{t("分机通话")}</span><Switch size="small" checked={account.allowCalls} disabled={busy} ariaLabel={`${t("分机通话")} ${account.username}`} onChange={allowCalls => patchAccount(account.key, { allowCalls })} /></div>
            <div className="mt-3 flex items-center justify-between gap-3 text-xs text-gray-500 dark:text-gray-400"><span>{t("收发分机消息")}</span><Switch size="small" checked={account.allowMessages} disabled={busy} ariaLabel={`${t("收发分机消息")} ${account.username}`} onChange={allowMessages => patchAccount(account.key, { allowMessages })} /></div>
            {!!form.carrierLines.length && <details className="mt-3"><summary className="cursor-pointer text-xs font-medium text-gray-500">{t("允许外呼线路（默认无权限）")}</summary><div className="mt-3 space-y-3">{form.carrierLines.filter(line => line.deviceId).map(line => <div key={line.key} className="flex items-center justify-between gap-3 text-xs text-gray-500"><span className="min-w-0 break-all">{line.label || line.deviceId}</span><Switch size="small" checked={account.outgoingLines.includes(line.deviceId)} disabled={busy || !account.allowCalls} ariaLabel={`${t("允许外呼")} ${account.username} ${line.label || line.deviceId}`} onChange={allowed => patchAccount(account.key, { outgoingLines: allowed ? [...account.outgoingLines, line.deviceId] : account.outgoingLines.filter(id => id !== line.deviceId) })} /></div>)}</div></details>}
          </div>)}
        </div>
        <details className="rounded-xl border border-gray-200 p-4 dark:border-white/10">
          <summary className="cursor-pointer text-sm font-medium">{t("高级设置")}</summary>
          <div className="mt-4 space-y-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <div><label htmlFor={`${id}-realm`} className={labelClass}>Realm</label><Input id={`${id}-realm`} value={form.realm} disabled={busy} onChange={e => setForm(previous => previous ? updateSIPRealm(previous, e.target.value) : previous)} /></div>
              <div><label htmlFor={`${id}-algorithm`} className={labelClass}>{t("认证算法")}</label><select id={`${id}-algorithm`} value={form.algorithm} disabled={busy} onChange={e => patch({ algorithm: e.target.value })} className="h-8 w-full rounded-lg border border-gray-300 bg-white px-3 text-sm dark:border-white/15 dark:bg-gray-900"><option value="SHA-256">SHA-256</option><option value="MD5">MD5 ({t("旧客户端兼容")})</option></select></div>
            </div>
            <div><label htmlFor={`${id}-domain`} className={labelClass}>{t("统一身份域（可选，不含端口）")}</label><Input id={`${id}-domain`} value={form.identityDomain} placeholder={form.realm} disabled={busy} onChange={e => patch({ identityDomain: e.target.value })} /><p className="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">{t("统一消息发件地址，留空使用 Realm。跨内网／VPN 时，所有软电话仍须配置指向同一服务器的出站代理；此域名不决定投递目的地。")}</p></div>
            <p className="text-xs text-gray-500">{t("修改 Realm 或分机号时，须重新填写对应密码。保存变更后，客户端需重新注册。")}</p>
            <div><label htmlFor={`${id}-networks`} className={labelClass}>{t("允许接入的网段（CIDR，每行一个）")}</label><Textarea id={`${id}-networks`} rows={5} value={form.networks} disabled={busy} onChange={e => patch({ networks: e.target.value })} /></div>
            <p className="text-xs leading-5 text-amber-600 dark:text-amber-400">{t("当前为未加密的 UDP SIP，仅用于可信内网或 VPN。请勿直接开放到公网；Digest 摘要也需作为密码保管。")}</p>
          </div>
        </details>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <span className="text-xs text-gray-500">{changed ? t("有未保存的修改") : t("开关与配置将在下次启动时自动恢复")}</span>
          <Button variant="primary" loading={busy} disabled={!changed && !status?.error} onClick={() => void save()}>{t("保存 SIP 配置")}</Button>
        </div>
      </>}
    </div>
  </section>;
}
