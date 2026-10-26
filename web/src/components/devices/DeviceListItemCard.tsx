import type { DeviceListItem } from "../../types";
import { cx } from "../../lib/utils";
import { Tag, StatusDot } from "../ui";
import { deviceStatusMeta } from "./shared";
import { deviceTypeImage } from "../../lib/deviceTypes";
import { hostIMSStatus } from "./imsLineModel";
import { useI18n } from "../../lib/i18n";

export interface DeviceListItemCardProps {
  device: DeviceListItem;
  selected: boolean;
  statusText: string;
  onSelect: (id: string) => void;
}

export function DeviceListItemCard({ device, selected, statusText, onSelect }: DeviceListItemCardProps) {
  const { t } = useI18n();
  const host = device.imsMode === "host_ims" ? hostIMSStatus(device) : null;
  const meta = host ? { ...host, label: t(host.label), tag: host.tone === "neutral" ? "info" as const : host.tone } : deviceStatusMeta(device);
  return (
    <div className="device-list-item">
      <button
        type="button"
        onClick={() => onSelect(device.id)}
        className={cx(
          "h-full w-full rounded-xl border p-3 text-left transition-all",
          selected
            ? "border-indigo-200 bg-indigo-50/70 dark:border-indigo-500/30 dark:bg-indigo-500/10"
            : "border-gray-100 hover:bg-gray-50/60 dark:border-white/10 dark:hover:bg-white/5",
        )}
      >
        <div className="flex items-start justify-between gap-2">
          <img src={deviceTypeImage(device.deviceType)} alt="" className="h-10 w-10 shrink-0 object-contain" />
          <div className="min-w-0 flex-1">
            <div className="truncate font-bold text-gray-800 dark:text-gray-100">{device.name || device.id}</div>
            <div className="mt-0.5 truncate text-xs text-gray-500">
              {device.id} · {device.interface || "--"}
            </div>
            <div className="mt-1 truncate text-xs text-gray-400">{statusText}</div>
          </div>
          <div className="flex items-center gap-2">
            <StatusDot tone={meta.tone} size="sm" animated={meta.animated} />
            <Tag type={meta.tag}>{meta.label}</Tag>
          </div>
        </div>
      </button>
    </div>
  );
}
