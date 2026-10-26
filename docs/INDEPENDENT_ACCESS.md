# 蜂窝／Wi-Fi 独立接入

VoWin 的基准为 `SimTelephonyPreferences`：蜂窝模式、蜂窝自动连接、Wi-Fi 自动连接和
默认路由分别保存。默认路由变化只影响新业务，已建立通话继续由原接入控制器处理。

VoCat 正在把应用层从 `IMSMode/IMSEnabled` 互斥模型迁移到逐卡独立接入。当前新增源码
尚未完成整合，也未进行最终统一测试／构建；以下区分已有运行时边界与应用层开发进展。

## Wi-Fi 运行时射频策略

`vowifi.Options.ResolveRadioPolicy` 在每次 Enable／Reconnect／Retry 时解析策略。
没有配置解析器时使用 `exclusive`，保持原有射频关闭和恢复流程。解析器返回空值、未知值
或读取错误会在 SIM／射频 I/O 前终止该次尝试。

| 策略 | Wi-Fi 运行时的射频所有权 | 连接、恢复与停止 |
| --- | --- | --- |
| `exclusive` | 捕获并持有原射频检查点 | 建立 RF-off 后读取 UICC；重试与重连沿用检查点；显式停止恢复原状态 |
| `coexist` | 不取得射频或蜂窝数据所有权 | 不调用 Snapshot、RF-off、StopCellularData 或 Restore；只创建和释放 Wi-Fi／IKE／IMS 资源 |

共存策略允许蜂窝所有者维持自己的射频、数据和注册。它不会自动开启蜂窝，也不证明特定
固件或运营商允许同一 SIM 双注册。AT／QMI 的身份、应用选择与 AKA 路径继续使用原适配器。

一次 Wi-Fi 事务捕获其策略。失败重试和手动重连不能把 `exclusive` 改成 `coexist`，也不能
把 `coexist` 改成 `exclusive`；调用者先通过 `StopDevice` 确认清理，再保存和启动新策略。
状态 `radio_policy` 和持久化运行记录 `extra_json.radio_policy` 保留实际事务的策略。

## 射频清理确认

恢复射频失败时，保留原检查点，状态为 `failed`、`enabled=false`、
`last_error_class=radio_cleanup_pending`。Enable／Reconnect 返回 `ErrCleanupIncomplete`，不会开始
另一接入。再次 Disable／StopDevice 重试原 Restore；成功后才能开始新的策略。

IMS 注销失败但本地资源已释放时，仍使用现有 cleanup warning 语义。射频恢复失败具有
保留所有权的语义；不能依靠一个空闲状态快照推断清理完成。管理层在被清理所有权拒绝时
退出当前 worker，按普通重试间隔处理，避免同步循环重复请求。

管理器首次 Close 立即禁止新工作。若射频恢复尚未确认，重复 Close 继续重试保留的
所有者；已经标记 closed 不能作为清理已完成的依据。

## 应用层开发进展（尚未统一验证）

- 数据库版本 30 加入 `devices.telephony_json` 和 `sim_telephony_policies`，保存 ICCID、
  蜂窝方式、两个自动连接标志、默认路由及 Wi-Fi 射频策略。逐卡偏好与当前设备投影原子保存。
- `GET/PATCH /api/devices/:id/telephony` 编辑实际 SIM 的配置；
  `POST /api/devices/:id/telephony/actions/reconnect` 按 `access` 分别重连。
  默认路由单独变化不触发停止、注销或重建已有媒体。
- Wi-Fi 工厂读取当前卡的策略；Native QMI 共存路径使用 proxy，不执行 CTL Sync、
  SIM 重置或射频恢复。Host IMS 的硬件保护依据蜂窝所有者，而不是默认呼出线路。
- 管理分派汇总两路 IMS，通话及 USSI 对话固定原所有者；未知或冲突的标识拒绝操作。
  默认线路关闭时新业务失败，不隐式改走其他接入或 SIM。
- 页面加入两个连接开关、蜂窝方式、默认路由、射频策略和分别重连；双运行时分别显示。
  Telegram、eSIM 切卡与自动任务正在接入同一配置边界。
- 独立配置的启动意愿改由逐卡协调器投递；不再先按旧端口状态启用。协调器按模组隔离，
  等待原运行时确认停止后才采用新卡策略；单路启动失败不会反复停止另一条已排队的接入。
  WiFi410 的 Wi-Fi 冷启动仍保留既有延迟。停止未确认的接入保留清理标记，后续继续重试。
- Host IMS 使用 ModemManager 的实时主 SIM 元数据，区分已确认无可用 SIM 与元数据暂时
  不可用；USB 代次变化先清理原所有者。页面及新通话准入使用当前身份，切回模块后等待
  新 AT 快照，避免旧缓存重新覆盖已采用的新卡。UI 元数据证明不代替写操作的身份核验。
- Host IMS 可通过绑定后的 `Modem.Enable(true)` 开启关闭的射频，再观察注册；不执行原始
  QMI 重置或接管外部承载。普通 DMS/NAS/UIM/WDS 客户端不再执行 CTL Sync；Host IMS
  或共存 Wi-Fi 配置下禁用自动 SIM 电源循环和 NAS 恢复中的射频循环。
- 模块启用、重连和独立配置下的手动飞行模式接入 `SetFlightForSIM`；设备层在自身 AT／QMI
  租约下以 ICCID→IMSI→ICCID 实读核对当前卡，检查当前配置和 USB 代次，再写入射频状态。
  RF-on 保留区域准入；RF-off 可用于隔离已核验的受限卡。NAS 注册 worker 固定原请求的
  身份；每次注册／搜索／附着／选网／射频写入和失败回滚前重复核验，SIM 绑定、蜂窝所有者或物理目标变化
  即停止。射频已在线但尚未注册时，可重新排队 NAS 协调，不依靠反复开关射频恢复。
  这些是新增源码约束，尚未通过本轮统一测试，也不表示固件换卡时序已经实机验收。
- 首次采用旧配置仍需在已识别的当前 SIM 上显式 Apply。没有历史 ICCID 的逐设备配置
  不能自动认作新卡的已保存偏好；新卡的独立配置来自其自身逐卡记录或默认值。

## 仍需完成

- 继续收尾旧配置迁移、冷启动、换卡／拔卡及 Host IMS 元数据不可用时的生命周期；
  补齐原数据会话清理、自动任务临时环境及失败恢复，并统一验证新增 RF／NAS 原卡核验。
- 完成并验证全部管理入口，补齐尚未接通的兼容路径；本开发提交不表示这些边界已经通过。
- 实现结束后统一执行回归、并发／取消、前端与多架构构建；随后验收双注册、断网、
  换卡、热拔、停止、冷启动和真实双向媒体。不要复用早先源码快照的结果作为本次结论。

运行时软件验证与构建见[本阶段记录](../../publish/vocat-independent-access-20261026/README.md)。
