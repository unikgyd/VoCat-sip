import { useEffect, useId, useState } from "react";
import { KeyRegular } from "@fluentui/react-icons";
import { api, apiMessage } from "../../api";
import { useI18n } from "../../lib/i18n";
import { Button, Input, message } from "../ui";
import { StatusDot } from "../ui/StatusDot";
import { Switch } from "../ui/Switch";
import { CardDecor, CardIcon, CardTitle } from "./Cards";
import { adoptSmsAPI, generateSmsAPIToken, smsAPIFormError, type SmsAPIForm, type SmsAPIStatus } from "./smsAPIModel";

export function SMSAPICard() {
  const { t } = useI18n();
  const id = useId();
  const [status, setStatus] = useState<SmsAPIStatus | null>(null);
  const [form, setForm] = useState<SmsAPIForm | null>(null);
  const [saved, setSaved] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [loadError, setLoadError] = useState(false);
  const [visible, setVisible] = useState(false);
  const [retry, setRetry] = useState(0);
  function adopt(data: SmsAPIStatus) {
    const next = adoptSmsAPI(data);
    setStatus(data); setForm(next); setSaved(JSON.stringify(next)); setVisible(false);
  }
  useEffect(() => {
    let active = true;
    setLoading(true); setLoadError(false);
    const controller = new AbortController();
    api<SmsAPIStatus>("/settings/sms-api", { signal: AbortSignal.any([controller.signal, AbortSignal.timeout(10000)]) }).then(data => { if (active) adopt(data); })
      .catch(() => { if (active) setLoadError(true); }).finally(() => { if (active) setLoading(false); });
    return () => { active = false; controller.abort(); };
  }, [retry]);
  const dirty = form !== null && JSON.stringify(form) !== saved;
  const error = form ? smsAPIFormError(form, status?.tokenConfigured === true) : null;
  const patch = (next: Partial<SmsAPIForm>) => setForm(previous => previous ? { ...previous, ...next } : null);
  const save = async () => {
    if (!form || error) return;
    setBusy(true);
    try {
      const data = await api<SmsAPIStatus>("/settings/sms-api", { method: "PUT", body: { ...form, port: Number(form.port) } });
      adopt(data);
      if (data.error) message.error(t("配置已保存，但接口未启动；请检查 IP 和端口后重试"));
      else message.success(t("API 配置已应用"));
    } catch (e) { message.error(apiMessage(e) || t("API 配置保存失败")); }
    finally { setBusy(false); }
  };
  const label = loading || busy ? "处理中" : loadError || status?.error ? "接口不可用" : dirty ? "未应用" : status?.running ? "运行中" : "已关闭";
  const tone = loading || busy ? "warning" : loadError || status?.error ? "danger" : dirty ? "warning" : status?.running ? "success" : "neutral";
  return <section className="ui-card group relative overflow-hidden p-6 sm:p-8" aria-labelledby={`${id}-title`}>
    <CardDecor />
    <div className="relative z-10 flex flex-wrap items-center justify-between gap-4">
      <div className="flex min-w-0 items-center gap-3"><CardIcon><KeyRegular className="text-[24px]" /></CardIcon><div id={`${id}-title`}><CardTitle title={t("短信与验证码 API")} subtitle={t("只读收件箱 · 独立 Token · 默认关闭")} /></div></div>
      <div className="flex items-center gap-3"><span role="status" className="inline-flex items-center gap-2 text-xs text-gray-500 dark:text-gray-400"><StatusDot tone={tone} animated={loading || busy} />{t(label)}</span><Switch ariaLabel={t("启用短信与验证码 API")} checked={form?.enabled ?? false} disabled={!form || busy || loading} onChange={enabled => patch({ enabled })} /></div>
    </div>
    {loadError ? <div className="relative z-10 mt-5 flex items-center justify-between gap-3 text-sm"><p>{t("API 配置加载失败")}</p><Button onClick={() => setRetry(value => value + 1)}>{t("重试")}</Button></div> : form && <fieldset disabled={busy || loading} className="relative z-10 mt-6 min-w-0 space-y-4">
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-[minmax(0,1fr)_8rem]">
        <label className="space-y-2 text-xs text-gray-500 dark:text-gray-400"><span>{t("监听 IP")}</span><Input value={form.bindAddress} placeholder="127.0.0.1" autoComplete="off" spellCheck={false} onChange={e => patch({ bindAddress: e.target.value })} /></label>
        <label className="space-y-2 text-xs text-gray-500 dark:text-gray-400"><span>{t("端口")}</span><Input value={form.port} inputMode="numeric" autoComplete="off" onChange={e => patch({ port: e.target.value })} /></label>
      </div>
      <label className="flex items-center gap-2 text-xs text-gray-600 dark:text-gray-300"><input type="checkbox" checked={form.allowLanAccess} onChange={e => patch({ allowLanAccess: e.target.checked })} />{t("允许局域网／VPN 访问（仅具体私网 IP）")}</label>
      <div className="space-y-2">
        <label htmlFor={`${id}-token`} className="text-xs text-gray-500 dark:text-gray-400">Token</label>
        <div className="flex flex-wrap gap-2"><div className="min-w-0 flex-1 basis-48"><Input id={`${id}-token`} type={visible ? "text" : "password"} value={form.token} placeholder={t(status?.tokenConfigured ? "已配置；留空保留现有 Token" : "生成或填写独立 Token")} autoComplete="new-password" spellCheck={false} maxLength={128} onChange={e => patch({ token: e.target.value })} /></div><Button onClick={() => setVisible(value => !value)}>{t(visible ? "隐藏 Token" : "显示 Token")}</Button><Button onClick={() => { patch({ token: generateSmsAPIToken(window.crypto) }); setVisible(true); }}>{t("生成新 Token")}</Button></div>
        {form.token && <p className="text-xs text-amber-700 dark:text-amber-300">{t("请先复制保存；应用后不再显示，旧 Token 将失效。")}</p>}
      </div>
      <p className="text-xs leading-5 text-gray-500 dark:text-gray-400">{t("Token 可读取全部线路的收件短信和验证码。不要分享给不受信任的调用者，不要直接映射 HTTP 到公网。")}</p>
      {error && dirty && <p role="alert" className="text-xs text-red-600 dark:text-red-400">{t(error)}</p>}
      {status?.error && <p role="alert" className="text-xs text-red-600 dark:text-red-400">{t("接口未启动，请检查本机 IP、端口占用或重新保存配置。")}</p>}
      <div className="flex flex-wrap items-center justify-between gap-3"><span className="min-w-0 break-all font-mono text-xs text-gray-500 dark:text-gray-400">{status?.baseUrl}</span><Button variant="primary" loading={busy} disabled={!!error} onClick={save}>{t("保存并应用 API")}</Button></div>
      <details className="border-t border-gray-200/60 pt-4 text-xs dark:border-white/10"><summary className="cursor-pointer text-gray-500 dark:text-gray-400">{t("接口用法与安全边界")}</summary><div className="mt-3 space-y-2 leading-6 text-gray-500 dark:text-gray-400">
        <p><code>GET /api/v1/sms?keyword=login&amp;limit=20</code><br /><code>GET /api/v1/otp?maxAgeSeconds=300</code><br /><code>Authorization: Bearer &lt;Token&gt;</code></p>
        <p>{t("支持 keyword、sender、slotId、since；最多查询最近 7 天。只查询已保存的收件箱，不发短信、不删除、不标记已读。")}</p>
        <p>{t("不提供 CORS；网页请由受信任后端调用。公网接入使用 VPN 或自行配置的 HTTPS 反向代理。")}</p>
      </div></details>
    </fieldset>}
  </section>;
}
