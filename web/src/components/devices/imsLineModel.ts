import type { DeviceListItem, IMSMode } from "../../types";

export function selectedIMSMode(device: Pick<DeviceListItem, "imsMode" | "vowifiEnabled">): IMSMode {
  return device.imsMode || (device.vowifiEnabled ? "vowifi" : "module");
}

export function hostIMSStatus(device: Pick<DeviceListItem, "imsEnabled" | "imsRuntime"> & Partial<Pick<DeviceListItem, "telephonyPolicy" | "cellularImsRuntime">>) {
  const rt = device.cellularImsRuntime ?? device.imsRuntime;
  const enabled = device.telephonyPolicy ? device.telephonyPolicy.cellularMode === "host_ims" && device.telephonyPolicy.autoConnectCellular : device.imsEnabled;
  if (!enabled) return { label: "已关闭", tone: "neutral" as const, animated: false };
  if (rt?.lastErrorClass || rt?.phase === "failed") return { label: "连接失败", tone: "danger" as const, animated: false };
  // A cellular line has no ePDG tunnel. TunnelReady=false is not a failure.
  if (rt?.imsReady && rt?.accessReady && rt?.simReady) return { label: "IMS 已注册", tone: "success" as const, animated: false };
  return { label: rt?.phase === "stopping" ? "正在停止" : "正在连接", tone: "warning" as const, animated: true };
}
