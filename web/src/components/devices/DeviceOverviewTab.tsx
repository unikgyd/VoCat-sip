import { useState } from "react";
import { CellLockDialog } from "./CellLockDialog";
import { useDeviceCellLock } from "./useDeviceCellLock";
import { OverviewNetworkCard } from "./OverviewNetworkCard";
import { OverviewVowifiCard } from "./OverviewVowifiCard";
import { OverviewSimPanel } from "./OverviewSimPanel";
import { OverviewNetworkPanel } from "./OverviewNetworkPanel";
import { OverviewTrafficChart } from "./OverviewTrafficChart";
import { OperatorSelectionDialog } from "./OperatorSelectionDialog";
import type { DeviceDetail } from "./types";
import { useI18n } from "../../lib/i18n";
import { isVoWiFiInUse } from "./shared";
import { EpdgDiscoveryPanel } from "./EpdgDiscoveryPanel";
import { IMSLinePanel, OverviewHostIMSCard } from "./IMSLinePanel";
import { selectedIMSMode } from "./imsLineModel";

export interface DeviceOverviewTabProps {
  device: DeviceDetail;
  simOperatorDisplay: string;
  customPhoneNumber?: string;
  trafficSpeedRx: string;
  trafficSpeedTx: string;
  trafficMinuteRx: string;
  trafficMinuteTx: string;
  e911Starting: boolean;
  onSetupE911: () => void;
  onRefresh: () => void | Promise<void>;
}

export function DeviceOverviewTab(props: DeviceOverviewTabProps) {
  const { t } = useI18n();
  const [operatorOpen, setOperatorOpen] = useState(false);
  const { device } = props;
  const imsMode = selectedIMSMode(device);
	const wifiCallingOnly = device.deviceType === "usb_sim_reader";
  const supportsCellLock = device.supportsCellLock === true;
  const cellLock = useDeviceCellLock(device.id, supportsCellLock);
  const cellLocked = !!cellLock.status?.target;
	const showNetworkDetails = !!device.developerEnabled && !wifiCallingOnly;
  return (
    <div className="space-y-4">
      <IMSLinePanel device={device} onUpdated={props.onRefresh} />
      <div className={`grid grid-cols-1 gap-4 ${showNetworkDetails ? "lg:grid-cols-3" : "lg:grid-cols-2"}`}>
        <div className="ui-panel-muted p-4">
          <div className="mb-3 text-xs font-bold uppercase tracking-wider text-gray-500">{t("运行状态")}</div>
		  {device.telephonyPolicy ? <div className="space-y-5">
            {!wifiCallingOnly && (device.telephonyPolicy.cellularMode === "host_ims" ? <OverviewHostIMSCard device={device} /> : <OverviewNetworkCard device={device} onOpenOperatorSelection={() => setOperatorOpen(true)} onOpenCellLock={supportsCellLock ? cellLock.openDialog : undefined} cellLocked={cellLocked} />)}
            <OverviewVowifiCard device={device} />
          </div> : imsMode === "host_ims" ? <OverviewHostIMSCard device={device} /> : (imsMode === "vowifi" || isVoWiFiInUse(device)) ? (
            <OverviewVowifiCard device={device} />
          ) : (
            <OverviewNetworkCard device={device} onOpenOperatorSelection={() => setOperatorOpen(true)}
              onOpenCellLock={supportsCellLock ? cellLock.openDialog : undefined} cellLocked={cellLocked} />
          )}
        </div>
        <OverviewSimPanel
          device={device}
          simOperatorDisplay={props.simOperatorDisplay}
          customPhoneNumber={props.customPhoneNumber}
          e911Starting={props.e911Starting}
          onSetupE911={props.onSetupE911}
        />
		{showNetworkDetails ? <OverviewNetworkPanel
          device={device}
          trafficMinuteRx={props.trafficMinuteRx}
          trafficMinuteTx={props.trafficMinuteTx}
          trafficSpeedRx={props.trafficSpeedRx}
          trafficSpeedTx={props.trafficSpeedTx}
		/> : null}
      </div>
      {(!!device.telephonyPolicy || imsMode === "vowifi") && <EpdgDiscoveryPanel deviceId={device.id} simKey={device.modem?.simInserted === false ? "" : device.modem?.iccid || device.vowifiRuntime?.iccid || ""} />}
      {device.developerEnabled && device.networkEnabled && device.id ? <OverviewTrafficChart deviceId={device.id} /> : null}
	  {device?.id && !wifiCallingOnly ? (
        <OperatorSelectionDialog
          open={operatorOpen}
          deviceId={device.id}
          scanBlockedReason={
            device.flightMode || device.modem?.operatingMode === 0 || device.modem?.operatingMode === 4
              ? t("运营商扫描需要开启蜂窝射频；请先关闭飞行模式，再手动开始扫描。")
              : ""
          }
          onClose={() => setOperatorOpen(false)}
          onUpdated={props.onRefresh}
        />
      ) : null}
      {supportsCellLock && cellLock.dialogOpen ? <CellLockDialog cellLock={cellLock} /> : null}
    </div>
  );
}
