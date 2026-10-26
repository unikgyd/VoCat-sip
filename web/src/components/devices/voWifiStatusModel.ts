import type { TelephonyPolicy, VoWiFiRuntime } from "../../types";

type Device = { id?: string; imsMode?: string; imsEnabled?: boolean; vowifiEnabled?: boolean;
  telephonyPolicy?: TelephonyPolicy | null;
  modem?: { iccid?: string; simInserted?: boolean }; vowifiRuntime?: Partial<VoWiFiRuntime> };
export interface VoWiFiStatusView {
  label: string; tone: "neutral" | "success" | "warning" | "danger"; animated: boolean;
  runtime?: Partial<VoWiFiRuntime>; reason?: string;
}

// Cached observations only: never derive retry permission from a browser clock.
export function voWiFiStatus(device: Device): VoWiFiStatusView {
  const wanted = device.telephonyPolicy?.autoConnectWifi ?? (device.imsMode === "vowifi" ? device.imsEnabled ?? device.vowifiEnabled : device.vowifiEnabled);
  const rt = device.vowifiRuntime;
  const currentCard = device.modem?.iccid?.trim();
  const mismatch = !!((device.id && rt?.deviceId && device.id !== rt.deviceId) ||
      (currentCard && rt?.iccid && currentCard !== rt.iccid.trim()));
  if (!wanted || rt?.enabled === false) return { label: "已关闭", tone: "neutral", animated: false, runtime: mismatch ? undefined : rt };
  if (mismatch)
    return { label: "状态待核验", tone: "danger", animated: false };
  if (device.modem?.simInserted === false) return { label: "SIM 未就绪", tone: "danger", animated: false };
  if (rt?.phase === "stopping") return { label: "正在停止", tone: "warning", animated: true, runtime: rt };
  // SMS capability is independent; not advertising SMS cannot invalidate voice.
  if (rt?.simReady && rt.accessReady && rt.tunnelReady && rt.imsReady && rt.phase !== "failed" && !rt.lastErrorClass && !rt.lastError)
    return { label: "IMS 已注册", tone: "success", animated: false, runtime: rt };
  const retry = rt?.ikeRetry;
  if (retry) {
    const seconds = retry.afterSeconds;
    const timed = !retry.untilCardEvent && Number.isSafeInteger(seconds) && seconds >= 0 && seconds <= 1116000 &&
      ["operator", "implementation-missing", "implementation-invalid"].includes(retry.source);
    if (timed && !retry.cleanupPending) return { label: retry.cached || seconds === 0 ? "状态待复核" : "等待重试", tone: "warning", animated: false, runtime: rt,
      reason: retry.source === "implementation-missing" ? "运营商未提供等待时长，正在使用本地保守等待。"
        : retry.source === "implementation-invalid" ? "运营商等待时长无效，正在使用本地保守等待。" : "运营商要求暂缓新的 IKE 握手。" };
    const reason = retry.source === "uncertain-attempt" ? "上次握手结果尚未确认，新握手已暂停，不能自动重放。"
      : retry.source === "store-unavailable" ? "无法确认本地重试记录或计时依据，新握手已暂停。"
      : retry.source === "binding-unavailable" ? "当前 SIM 或尝试的归属未能确认，新握手已暂停。"
      : retry.cleanupPending ? "旧连接清理尚未完成，等待结束后仍需重新核验。"
      : retry.untilCardEvent && retry.source === "operator" ? "运营商限制仍需核验；普通重启或手动重试不会清除记录。"
      : "重试状态尚未确认，请查看诊断日志。";
    return { label: "已暂停", tone: "danger", animated: false, runtime: rt, reason };
  }
  if (rt?.lastErrorClass || rt?.lastError || rt?.phase === "failed") return { label: "连接失败", tone: "danger", animated: false, runtime: rt };
  return { label: rt?.phase === "idle" || !rt ? "等待连接" : "正在连接", tone: "warning", animated: true, runtime: rt };
}
