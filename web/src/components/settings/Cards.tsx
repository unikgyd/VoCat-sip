import type { ReactNode } from "react";
import {
  AlertRegular,
  CheckmarkRegular,
  InfoRegular,
  KeyRegular,
} from "@fluentui/react-icons";
import type { SystemInfo } from "../../types";
import { useI18n } from "../../lib/i18n";
import { Button } from "../ui/Button";
import { StatusDot } from "../ui/StatusDot";
import { FieldRow, PasswordInput } from "./controls";

export interface PasswordForm {
  oldPassword: string;
  newPassword: string;
  confirmPassword: string;
}

export interface UpdateInfo {
  hasUpdate?: boolean;
  latestVersion?: string;
  releaseNote?: string;
  isDocker?: boolean;
}

function CardDecor() {
  return (
    <div className="absolute right-0 top-0 -mr-10 -mt-10 h-40 w-40 rounded-bl-full bg-indigo-500/5 transition-transform group-hover:scale-110" />
  );
}

function CardIcon({ children, small }: { children: ReactNode; small?: boolean }) {
  return (
    <div
      className={
        small
          ? "flex h-9 w-9 items-center justify-center rounded-xl bg-indigo-50 text-indigo-600 dark:bg-indigo-500/10 dark:text-indigo-400"
          : "flex h-12 w-12 items-center justify-center rounded-xl bg-indigo-50 text-indigo-600 dark:bg-indigo-500/10 dark:text-indigo-400"
      }
    >
      {children}
    </div>
  );
}

function CardTitle({ title, subtitle }: { title: string; subtitle: string }) {
  return (
    <div>
      <h3 className="text-lg font-bold text-gray-800 dark:text-gray-100">{title}</h3>
      <p className="text-xs text-gray-500">{subtitle}</p>
    </div>
  );
}

const PASSWORD_LABEL = "text-xs font-bold uppercase tracking-wider text-gray-500";

export function SecurityCard({
  value,
  onChange,
  loading,
  onSubmit,
}: {
  value: PasswordForm;
  onChange: (patch: Partial<PasswordForm>) => void;
  loading: boolean;
  onSubmit: () => void;
}) {
  const { t } = useI18n();
  return (
    <div className="ui-card group relative overflow-hidden p-8">
      <CardDecor />
      <div className="relative z-10 mb-6 flex items-center gap-3">
        <CardIcon>
          <KeyRegular className="text-[24px]" />
        </CardIcon>
        <CardTitle title={t("安全")} subtitle={t("更新访问凭证")} />
      </div>
      <div className="relative z-10 space-y-4">
        <div className="space-y-1">
          <label className={PASSWORD_LABEL}>{t("当前密码")}</label>
          <PasswordInput
            inputSize="large"
            placeholder="••••••••"
            autoComplete="current-password"
            value={value.oldPassword}
            onChange={(oldPassword) => onChange({ oldPassword })}
          />
        </div>
        <div className="space-y-1">
          <label className={PASSWORD_LABEL}>{t("新密码")}</label>
          <PasswordInput
            inputSize="large"
            placeholder="••••••••"
            autoComplete="new-password"
            value={value.newPassword}
            onChange={(newPassword) => onChange({ newPassword })}
          />
        </div>
        <div className="space-y-1">
          <label className={PASSWORD_LABEL}>{t("确认新密码")}</label>
          <PasswordInput
            inputSize="large"
            placeholder="••••••••"
            autoComplete="new-password"
            value={value.confirmPassword}
            onChange={(confirmPassword) => onChange({ confirmPassword })}
          />
        </div>
        <div className="pt-4">
          <Button variant="primary" size="large" loading={loading} onClick={onSubmit} className="w-full !border-0" icon={<CheckmarkRegular />}>
            {t("更新凭证")}
          </Button>
        </div>
      </div>
    </div>
  );
}

export function SystemInfoCard({
  info,
  updateInfo,
  checkingUpdate,
  applyingUpdate,
  onCheckUpdate,
  onApplyUpdate,
}: {
  info: SystemInfo;
  updateInfo: UpdateInfo | null;
  checkingUpdate: boolean;
  applyingUpdate: boolean;
  onCheckUpdate: () => void;
  onApplyUpdate: () => void;
}) {
  const { t } = useI18n();
  return (
    <div className="ui-card group relative overflow-hidden p-8">
      <CardDecor />
      <div className="relative z-10 mb-6 flex items-center gap-3">
        <CardIcon>
          <InfoRegular className="text-[24px]" />
        </CardIcon>
        <CardTitle title={t("系统信息")} subtitle={t("运行环境")} />
      </div>
      <div className="relative z-10 space-y-4 text-sm">
        <div className="rounded-lg bg-gray-50 p-3 dark:bg-white/5">
          <FieldRow label={t("版本")} value={info.version} monospace>
            <div className="flex items-center justify-end gap-3">
              <Button size="small" variant="primary" className="!border-0" loading={checkingUpdate} onClick={onCheckUpdate}>
                {t("检查更新")}
              </Button>
              <span>{info.version || "Unknown"}</span>
            </div>
          </FieldRow>
        </div>
        {updateInfo?.hasUpdate ? (
          <div className="rounded-lg border border-amber-200 bg-amber-50 p-4 dark:border-amber-500/20 dark:bg-amber-500/10">
            <div className="mb-2 flex items-center gap-2 text-[13px] font-bold text-amber-800 dark:text-amber-200">
              <AlertRegular /> {t("发现新版本:")} {updateInfo.latestVersion}
            </div>
            <div className="mb-4 max-h-32 overflow-y-auto whitespace-pre-wrap pr-2 text-xs text-amber-700 dark:text-amber-300/80">
              {updateInfo.releaseNote || t("暂无更新说明")}
            </div>
            <Button variant="warning" loading={applyingUpdate} onClick={onApplyUpdate} className="w-full !border-0">
              {t("立即更新并重启")}
            </Button>
          </div>
        ) : null}
        <div className="rounded-lg bg-gray-50 p-3 dark:bg-white/5">
          <FieldRow label={t("构建时间")} value={info.buildTime} monospace />
        </div>
        <div className="rounded-lg bg-gray-50 p-3 dark:bg-white/5">
          <FieldRow label={t("配置路径")} value={info.config} monospace copyable />
        </div>
        <div className="rounded-lg bg-gray-50 p-3 dark:bg-white/5">
          <FieldRow label={t("运行时长")} value={info.uptime} monospace />
        </div>
        <div className="rounded-lg bg-gray-50 p-3 dark:bg-white/5">
          <FieldRow label="OS" value={info.os} monospace />
        </div>
        <div className="rounded-lg bg-gray-50 p-3 dark:bg-white/5">
          <FieldRow label={t("架构")} value={info.architecture} monospace />
        </div>
          {info.mediaCapabilities && <div className="rounded-lg bg-gray-50 p-3 dark:bg-white/5">
            <FieldRow label="AMR-NB"><span className="inline-flex items-center gap-2"><StatusDot tone={info.mediaCapabilities.amrNb.available ? "success" : "danger"} animated={false}/>{t(info.mediaCapabilities.amrNb.available ? "编解码可用" : "编解码不可用")}</span></FieldRow>
            {!info.mediaCapabilities.amrNb.available && <details className="mt-3 text-xs leading-6 text-gray-500 dark:text-gray-400"><summary className="cursor-pointer">{t("查看媒体依赖")}</summary><p className="mt-2">{t(info.mediaCapabilities.amrNb.built ? "请安装匹配架构的 OpenCORE-AMR-NB 运行库后重试；Debian／Ubuntu 包名为 libopencore-amrnb0。" : "当前是静态／不支持的平台构建，仅提供 G.711。需要 AMR 语音时请使用 glibc-AMR 版本。")}</p><code>vocat media-check --require-amr --json</code></details>}
          </div>}
      </div>
    </div>
  );
}

export { CardDecor, CardIcon, CardTitle };
