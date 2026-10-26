import type { CarrierLine, CarrierLineForm } from "./CarrierLinesForm";

export interface SIPAccount { username: string; displayName: string; password?: string; hasPassword: boolean; resetCredentialIdentity?: boolean; allowMessages: boolean; allowCalls: boolean; outgoingLines: string[]; smsSendLines: string[] }
export interface PeerAuth { username: string; password?: string; hasPassword: boolean }
export interface PeerOutboundAuth extends PeerAuth { realm: string; algorithms: string[] }
export interface PeerIncomingRoute { deviceId: string; callTarget: string; smsTarget: string }
export interface SIPPeer { enabled: boolean; endpoint: string; inboundAuth: PeerAuth; outboundAuth: PeerOutboundAuth; allowCallsFromPeer: boolean; allowSmsFromPeer: boolean; allowCallsToPeer: boolean; allowSmsToPeer: boolean; outgoingLines: string[]; smsSendLines: string[]; incomingRoutes: PeerIncomingRoute[] }
export interface SIPPeerRuntime { enabled: boolean; endpoint: string; state: "disabled" | "configured" | "unavailable" }
export interface SIPConfig { enabled: boolean; listen: string; realm: string; identityDomain: string; messagesEnabled: boolean; callsEnabled: boolean; algorithm: string; allowedCidrs: string[]; accounts: SIPAccount[]; carrierEnabled: boolean; carrierLines: CarrierLine[]; smsEnabled: boolean; peer?: SIPPeer | null }
export interface SIPForm extends Omit<SIPConfig, "accounts" | "allowedCidrs" | "carrierLines" | "peer"> { networks: string; accounts: (SIPAccount & { key: string })[]; carrierLines: CarrierLineForm[]; peer: SIPPeer | null }

export function emptySIPPeer(): SIPPeer {
  return { enabled: false, endpoint: "", inboundAuth: { username: "", password: "", hasPassword: false }, outboundAuth: { username: "", realm: "", password: "", hasPassword: false, algorithms: ["SHA-256"] }, allowCallsFromPeer: false, allowSmsFromPeer: false, allowCallsToPeer: false, allowSmsToPeer: false, outgoingLines: [], smsSendLines: [], incomingRoutes: [] };
}

export function cloneSIPPeer(peer?: SIPPeer | null, clearPasswords = false): SIPPeer | null {
  if (!peer) return null;
  const defaults = emptySIPPeer();
  return { ...defaults, ...peer, inboundAuth: { ...defaults.inboundAuth, ...peer.inboundAuth, password: clearPasswords ? "" : peer.inboundAuth?.password || "" }, outboundAuth: { ...defaults.outboundAuth, ...peer.outboundAuth, password: clearPasswords ? "" : peer.outboundAuth?.password || "", algorithms: peer.outboundAuth?.algorithms?.length ? [...peer.outboundAuth.algorithms] : ["SHA-256"] }, outgoingLines: [...(peer.outgoingLines || [])], smsSendLines: [...(peer.smsSendLines || [])], incomingRoutes: (peer.incomingRoutes || []).map(route => ({ ...route, callTarget: route.callTarget || "", smsTarget: route.smsTarget || "" })) };
}

export function adoptSIPConfig(config: SIPConfig, createKey: () => string): SIPForm {
  const { allowedCidrs, accounts, carrierLines, peer, ...base } = config;
  return { ...base, identityDomain: base.identityDomain || "", messagesEnabled: base.messagesEnabled === true, callsEnabled: base.callsEnabled === true, carrierEnabled: base.carrierEnabled === true, smsEnabled: base.smsEnabled === true, peer: cloneSIPPeer(peer, true), networks: (allowedCidrs || []).join("\n"), carrierLines: (carrierLines || []).map(line => ({ ...line, incomingExtensions: [...(line.incomingExtensions || [])], smsRecipients: [...(line.smsRecipients || [])], allowIncoming: line.allowIncoming === true, allowSmsSend: line.allowSmsSend === true, allowSmsReceive: line.allowSmsReceive === true, key: createKey() })), accounts: (accounts || []).map(account => ({ ...account, outgoingLines: [...(account.outgoingLines || [])], smsSendLines: [...(account.smsSendLines || [])], allowCalls: account.allowCalls === true, allowMessages: account.allowMessages === true, password: "", resetCredentialIdentity: false, key: createKey() })) };
}

export function updateSIPAccount<T extends SIPAccount>(account: T, change: Partial<SIPAccount>): T {
  const renamed = change.username !== undefined && change.username !== account.username;
  return { ...account, ...change, hasPassword: renamed ? false : account.hasPassword, resetCredentialIdentity: renamed ? false : change.resetCredentialIdentity ?? account.resetCredentialIdentity };
}

export function updateSIPRealm(form: SIPForm, realm: string): SIPForm {
  if (realm === form.realm) return form;
  return { ...form, realm, accounts: form.accounts.map(account => ({ ...account, hasPassword: false, resetCredentialIdentity: false })), peer: form.peer ? { ...form.peer, inboundAuth: { ...form.peer.inboundAuth, hasPassword: false } } : null };
}

export function serializeSIPForm(form: SIPForm): SIPConfig {
  const { networks, accounts, carrierLines, peer, ...base } = form;
  return { ...base, peer: cloneSIPPeer(peer), allowedCidrs: networks.split(/[\n,]+/).map(value => value.trim()).filter(Boolean), accounts: accounts.map(({ key: _key, ...account }) => ({ ...account, outgoingLines: [...account.outgoingLines], smsSendLines: [...account.smsSendLines] })), carrierLines: carrierLines.map(({ key: _key, ...line }) => ({ ...line, incomingExtensions: [...line.incomingExtensions], smsRecipients: [...line.smsRecipients] })) };
}

export function updatePeerInbound(peer: SIPPeer, change: Partial<PeerAuth>): SIPPeer {
  return { ...peer, inboundAuth: { ...peer.inboundAuth, ...change, hasPassword: change.username !== undefined && change.username !== peer.inboundAuth.username ? false : peer.inboundAuth.hasPassword } };
}
export function updatePeerOutbound(peer: SIPPeer, change: Partial<PeerOutboundAuth>): SIPPeer {
  const identityChanged = change.username !== undefined && change.username !== peer.outboundAuth.username || change.realm !== undefined && change.realm !== peer.outboundAuth.realm;
  return { ...peer, outboundAuth: { ...peer.outboundAuth, ...change, hasPassword: identityChanged ? false : peer.outboundAuth.hasPassword } };
}
export function restrictPeerLines(peer: SIPPeer | null, deviceIds: Set<string>): SIPPeer | null {
  return peer ? { ...peer, outgoingLines: peer.outgoingLines.filter(id => deviceIds.has(id)), smsSendLines: peer.smsSendLines.filter(id => deviceIds.has(id)), incomingRoutes: peer.incomingRoutes.filter(route => deviceIds.has(route.deviceId)) } : null;
}

// Runtime presentation must not derive from the editable or last-saved form.
// A separate admin page can change the peer while this page keeps a draft.
export function sipPeerStatus(status: { running: boolean; error?: string; peer?: SIPPeerRuntime }, stale = false): { label: string; tone: "neutral" | "warning" | "danger" } {
  if (stale || !status.peer) return { label: "中继状态暂不可用", tone: "warning" };
  if (status.error && (status.peer.enabled || status.peer.state === "unavailable")) return { label: "中继配置错误", tone: "danger" };
  if (!status.peer.enabled || status.peer.state === "disabled") return { label: "中继已关闭", tone: "neutral" };
  if (!status.running) return { label: "中继已停用（SIP 服务关闭）", tone: "neutral" };
  if (status.peer.state !== "configured") return { label: "中继状态暂不可用", tone: "warning" };
  return { label: "配置有效（未验证连通）", tone: "warning" };
}

// This is form feedback, not a replacement for authoritative server validation.
function privateLiteralEndpoint(value: string): boolean {
  const match = /^(?:\[([^\]]+)\]|([^:]+)):(\d{1,5})$/.exec(value);
  if (!match || Number(match[3]) < 1 || Number(match[3]) > 65535) return false;
  if (match[1]) {
    const host = match[1].toLowerCase();
    if (host.includes("%")) return false;
    try {
      const parsed = new URL(`http://[${host}]/`).hostname;
      return parsed === "[::1]" || /^\[f[cd][0-9a-f]{2}:/.test(parsed);
    } catch { return false; }
  }
  if (!/^(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2})){3}$/.test(match[2])) return false;
  const parts = match[2].split(".").map(Number);
  return parts.every(part => part <= 255) && (parts[0] === 127 || parts[0] === 10 || parts[0] === 192 && parts[1] === 168 || parts[0] === 172 && parts[1] >= 16 && parts[1] <= 31);
}
export function sipPeerFormError(peer: SIPPeer | null): string | null {
  if (!peer) return null;
  if (!peer.enabled && !peer.endpoint && !peer.inboundAuth.username && !peer.inboundAuth.password && !peer.outboundAuth.username && !peer.outboundAuth.realm && !peer.outboundAuth.password && !peer.allowCallsFromPeer && !peer.allowSmsFromPeer && !peer.allowCallsToPeer && !peer.allowSmsToPeer && !peer.outgoingLines.length && !peer.smsSendLines.length && !peer.incomingRoutes.length) return null;
  if (!privateLiteralEndpoint(peer.endpoint)) return "中继端点须为可信内网或回环 IP:端口；IPv6 请加方括号。";
  const needsInbound = peer.allowCallsFromPeer || peer.allowSmsFromPeer;
  const needsOutbound = peer.allowCallsFromPeer || peer.allowCallsToPeer || peer.allowSmsToPeer;
  for (const [auth, required] of [[peer.inboundAuth, needsInbound], [peer.outboundAuth, needsOutbound]] as const) {
    const size = new TextEncoder().encode(auth.password || "").length;
    if (size > 0 && (size < 12 || size > 128) || (required || auth.username || size > 0) && (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(auth.username) || !auth.hasPassword && size === 0)) return "启用的中继方向须配置独立账号及 12–128 字节密码。";
  }
  if ((needsOutbound || peer.outboundAuth.username || peer.outboundAuth.password) && (!peer.outboundAuth.realm || !peer.outboundAuth.algorithms.length)) return "请填写出站预期 Realm，并明确允许的认证算法。";
  if (peer.incomingRoutes.some(route => !route.deviceId || !route.callTarget && !route.smsTarget || [route.callTarget, route.smsTarget].some(target => target && !/^[A-Za-z0-9+][A-Za-z0-9_.+\-]{0,63}$/.test(target)))) return "中继目标仅填写 user（最多 64 字符），不要填写完整 SIP URI。";
  if (peer.outboundAuth.algorithms.some(algorithm => algorithm !== "SHA-256" && algorithm !== "MD5")) return "中继认证算法仅支持 SHA-256 或显式开启的 MD5。";
  return null;
}
