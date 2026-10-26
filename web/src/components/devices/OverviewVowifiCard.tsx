import { useEffect, useId, useState } from "react";
import { cx } from "../../lib/utils";
import { StatusDot } from "../ui";
import { FieldRow } from "./FieldRow";
import type { DeviceDetail } from "./types";
import { tf, useI18n } from "../../lib/i18n";
import { voWiFiStatus } from "./voWifiStatusModel";

export function OverviewVowifiCard({ device }: { device: DeviceDetail }) {
  const { t } = useI18n();
  const [expanded, setExpanded] = useState(false);
  const detailsId = useId();
  useEffect(() => { setExpanded(false); }, [device.id, device.modem?.iccid]);
  const status = voWiFiStatus(device);
  const rt = status.runtime;
  const items = [
    { key: "SIM", ready: rt?.simReady },
    { key: "Access", ready: rt?.accessReady },
    { key: "Tunnel", ready: rt?.tunnelReady },
    { key: "IMS", ready: rt?.imsReady },
    { key: "SMS", ready: rt?.smsReady },
  ];
  const overall = status.tone;
  const epdg = typeof rt?.tunnel?.epdg === "string" ? rt.tunnel.epdg : "";
  const epdgAddress = typeof rt?.tunnel?.epdgAddress === "string" ? rt.tunnel.epdgAddress : "";
  const eapRejected = rt?.lastErrorClass === "eap_authentication_rejected";
  const showDetails = expanded;

  return (
    <>
      <div
        role="status" aria-label={`VoWiFi · ${t(status.label)}`}
        className={cx(
          "mb-3 flex items-center gap-2.5 rounded-xl border px-3.5 py-2.5",
          overall === "success" && "border-emerald-200 bg-emerald-50 dark:border-emerald-500/25 dark:bg-emerald-500/10",
          overall === "warning" && "border-amber-200 bg-amber-50 dark:border-amber-500/25 dark:bg-amber-500/10",
          overall === "danger" && "border-red-200 bg-red-50 dark:border-red-500/25 dark:bg-red-500/10",
          overall === "neutral" && "border-gray-200 bg-gray-50 dark:border-white/10 dark:bg-white/5",
        )}
      >
        <StatusDot tone={overall} size="sm" animated={status.animated} />
        <div className="min-w-0">
          <div
            className={cx(
              "text-sm font-bold leading-tight",
              overall === "success" && "text-emerald-700 dark:text-emerald-300",
              overall === "warning" && "text-amber-700 dark:text-amber-300",
              overall === "danger" && "text-red-700 dark:text-red-300",
              overall === "neutral" && "text-gray-500",
            )}
          >
            {overall === "success" ? "VoWiFi" : `VoWiFi · ${t(status.label)}`}
          </div>
        </div>
      </div>
      {epdg && <div className="mb-3 break-all text-xs leading-5 text-gray-500"><span>{t(overall === "success" ? "当前 ePDG" : rt?.phase === "failed" || !rt?.enabled || rt?.ikeRetry?.cached ? "最近尝试的 ePDG" : "正在尝试 ePDG")}</span><div className="font-mono">{epdg}</div>{epdgAddress && <div className="font-mono">{epdgAddress}</div>}</div>}
      {showDetails ? <div className="mb-3">
        <div className="mb-1 flex gap-1">
          {items.map((item) => (
            <div
              key={item.key}
              className={cx(
                "h-1.5 flex-1 rounded-full",
                item.ready === true
                  ? "bg-emerald-500 dark:bg-emerald-400"
                  : "bg-gray-200 dark:bg-white/10",
              )}
            />
          ))}
        </div>
        <div className="flex justify-between">
          {items.map((item) => (
            <span
              key={item.key}
              className={cx(
                "flex-1 text-center text-[10px]",
                "text-gray-400 dark:text-gray-500",
              )}
            >
              {item.key}
            </span>
          ))}
        </div>
      </div> : null}
      <div className="overflow-hidden rounded-lg border border-gray-200 dark:border-white/10">
        <button
          type="button"
          aria-expanded={showDetails} aria-controls={detailsId}
          className="flex w-full items-center justify-between px-3 py-2 text-xs text-gray-500 transition-colors hover:bg-gray-50 dark:text-gray-400 dark:hover:bg-white/5"
          onClick={() => setExpanded((v) => !v)}
        >
          <span className="font-bold uppercase tracking-wider">{t("详情")}</span>
          <span>{showDetails ? "▴" : "▾"}</span>
        </button>
        {showDetails ? (
          <div id={detailsId} className="space-y-1.5 border-t border-gray-100 px-3 pb-2 pt-2 text-sm text-gray-700 dark:border-white/5 dark:text-gray-200">
            {status.reason && <p className="break-words text-xs leading-5">{t(status.reason)}</p>}
            {rt?.ikeRetry && <div className="space-y-1.5 text-xs">
              {!rt.ikeRetry.untilCardEvent && Number.isSafeInteger(rt.ikeRetry.afterSeconds) && rt.ikeRetry.afterSeconds >= 0 &&
                <FieldRow label={t("最近核对等待")} value={tf("{seconds} 秒（最近核对值，非实时倒计时）", { seconds: rt.ikeRetry.afterSeconds })} />}
              <FieldRow label={t("状态来源")} value={t(rt.ikeRetry.cached ? "历史记录，待运行实例重新核验" : "当前运行实例的最近观察")} />
              <FieldRow label={t("作用域")} value={rt.ikeRetry.scope || "--"} monospace />
              {rt.ikeRetry.notifyType > 0 && <FieldRow label={t("原因码")} value={String(rt.ikeRetry.notifyType)} monospace />}
              <p className="break-words leading-5 text-gray-500">{t("更改地址、网络或手动重试不会清空限制；重试前会再次核验。")}</p>
            </div>}
            {eapRejected ? (
              <div className="mb-2 rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-xs leading-5 text-amber-800 dark:border-amber-500/25 dark:bg-amber-500/10 dark:text-amber-200">
                {t("已连接到运营商 ePDG，但 EAP-AKA 流程被拒绝。可能是初始身份、运营商 IKE/EAP 兼容性或订阅策略问题；请根据错误详情确认失败阶段。")}
              </div>
            ) : null}
            <FieldRow label={t("数据平面")} value={rt?.dataplaneMode || "--"} monospace />
            <FieldRow
              label={t("运营商配置")}
              value={!rt?.carrierProfile || rt.carrierProfile === "standard-3gpp" ? "3GPP Standard" : rt.carrierProfile}
              monospace
              copyable
            />
            <FieldRow label={t("匹配依据")} value={rt?.carrierProfileFrom || "standard"} monospace />
            <FieldRow label={t("最后原因")} value={rt?.lastReason || "--"} />
            <FieldRow label={t("错误分类")} value={rt?.lastErrorClass || "--"} monospace copyable />
            {rt?.lastError ? <FieldRow label={t("错误详情")} value={rt.lastError} monospace copyable /> : null}
          </div>
        ) : null}
      </div>
    </>
  );
}
