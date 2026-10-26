export interface SmsAPIStatus {
  enabled: boolean;
  bindAddress: string;
  port: number;
  allowLanAccess: boolean;
  tokenConfigured: boolean;
  running: boolean;
  error?: string;
  baseUrl: string;
}
export interface SmsAPIForm {
  enabled: boolean;
  bindAddress: string;
  port: string;
  allowLanAccess: boolean;
  token: string;
}
export function adoptSmsAPI(status: SmsAPIStatus): SmsAPIForm {
  return { enabled: status.enabled, bindAddress: status.bindAddress, port: String(status.port), allowLanAccess: status.allowLanAccess, token: "" };
}
export function smsAPIFormError(form: SmsAPIForm, tokenConfigured: boolean): string | null {
  if (!/^[0-9]+$/.test(form.port) || Number(form.port) < 1024 || Number(form.port) > 65535) return "端口须为 1024–65535";
  if (!form.bindAddress || form.bindAddress !== form.bindAddress.trim() || ["0.0.0.0", "::", "*"].includes(form.bindAddress)) return "请填写具体的本机回环或内网 IP";
  if (form.token && !/^[A-Za-z0-9_-]{32,128}$/.test(form.token)) return "Token 须为 32–128 位字母、数字、下划线或连字符";
  if (form.enabled && !form.token && !tokenConfigured) return "启用前请配置独立 Token";
  return null;
}
export function generateSmsAPIToken(crypto: Pick<Crypto, "getRandomValues">): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  return btoa(String.fromCharCode(...bytes)).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}
