# 蜂窝 UIM／ISIM／AKA 事务前置层

2026-10-23：已实现并测试绑定活动 bearer 租约的蜂窝 SIM 事务、严格 QMI UIM
协议适配器，以及卡内 IMS 身份传递。后续新增[Linux QMI 端点连接器](HOST_IMS_ENDPOINT.md)，
但**尚未接入业务运行时／API／UI，也未做真实 UICC 验收**。不要将这些合成测试视为实卡注册或已可启用的 Host IMS。
Linux 测试仅部署在用户指定的 `root@10.0.0.2`，不使用 WSL。

## 身份与应用选择

- 对整个 Card Status 做有界校验，用 GW-primary 索引选订阅，不默认第一张卡／第一个应用。
  主订阅须为 ready USIM，使用完整 AID；有 ISIM 但其不可用／存在歧义时拒绝降级。
- Slot Status 将逻辑订阅映射到物理槽，并核对 ModemManager 的槽位和选定 ICCID。
  多槽时不猜物理槽号。只有明确“不支持查询”的 QMI 错误、单卡订阅及槽 1 条件同时
  满足，才允许用显式 CardSlot1 读取 EF.ICCID；超时、畸形响应和其他错误均不回退。
- 只打开自有逻辑通道（1–19），不改基本通道、不输 PIN、不切卡槽、不变更 profile，
  也不使用 VoWiFi 的 RF-off 准备流程。该层本身不启停模组内置 IMS。
- 读取 USIM EF.IMSI 并与所选 MM 身份匹配；EF.AD 只接受明确的两位／三位 MNC。
  ISIM 存在时读取 EF.IMPI、EF.DOMAIN、首个 EF.IMPU；身份不可读就失败，不猜身份。
  只有 ISIM 确认不存在时，才用 IMSI 与已知 MNC 长度构造 USIM 退路。
- IMS `Config.HomeDomain` 接受完整卡内身份三元组。域名独立于 IMPI／IMPU 的 host 部分；
  例如私有身份与服务域不同的卡不应被套入通用 PLMN 域。缺少 EF.AD 但 ISIM 三个文件
  完整时无需猜 MNC；该路径不改变原 VoWiFi PLMN／运营商档案默认行为。

## 生命周期与密钥边界

- 操作共用 bearer 的逐模组门，先持久保存 `SIMPending`，再打开 UIM 传输。
  同一模组多个适配器不能并发；不同模组可独立鉴权。
- 每次 AKA 前重读完整身份，核对相同 ICCID／IMSI／槽位／应用／卡内身份，以及活动
  bearer 的接口、地址和 multiplex 状态。身份变化后不把挑战发给另一个应用或另一张卡。
- 挑战摘要在 AUTH 前持久保存。当前防重范围是**同一 bearer 租约**，跨适配器实例有效，
  每租约最多 128 个摘要；达到上限拒绝新挑战。正常释放租约会清除该历史，不能宣称
  这是永久的跨租约 SIM 挑战历史。未知结果的记录不能通过重启／Recover 自动抹去。
- AUTH 超时、6Cxx、异常状态都不重新发 AUTH；61xx／9Fxx 只获取已有响应。
  SELECT／READ 的明确 6Cxx 可修正 Le，GET RESPONSE 使用相同逻辑通道并有循环上限。
- 调用方取消可及时返回；独立后台事务继续观察回复并尝试受限收尾。
  通道或传输清理不确定时保留句柄，持久写 `SIMUncertain`，禁止自动重试／释放 bearer。
  当前没有强制忘记或自动修复未知通道状态的入口。
- 只有完整响应校验、卡／承载复核、通道关闭与传输释放确认后才交付 RES／CK／IK 或 AUTS。
  错误、取消或清理失败时擦除结果；诊断不回显原始驱动异常、身份或 APDU。

## 与 Windows 的双向补齐

共享 `usim-aka-response-v1` 和 `sim-identity-bcd-v1` 既有合成契约；新增 Go／C#
`isim-identity-v1`：严格 UTF-8、长度边界、仅 FF 尾部填充，拒绝空白／控制符／第二个 TLV。
先失败测试复现并修复 C# 的 8 个问题用例。VoSharp ISIM 读取器不再忽略 AT+CCHC 失败，
清理失败时不交付身份／密钥，同一读取器不再开启新通道；这**不是**跨所有桌面实例的
持久通道恢复机制。201 份共享 C# 源镜像已同步，未替换正在使用的 VoWin 程序。

扩大 C# 回归还发现旧 ESP 回环测试使用保留 SPI 100／200。仅将合成 SPI 改为有效值，
没有放宽生产 ESP 校验。

## 验证范围与下一步

已覆盖合成多卡索引与槽位映射、身份变化、USIM 退路、AKA 成功／AUTS／错误状态、
不重放、取消／迟到回复、通道与 CID 保留、日志持久化失败、进程重启记录、多模组并行、
APDU 纠错／响应链、QMI 响应关联和畸形 TLV。另运行 Go race、原生 Linux 回归及解析器 fuzz。
QMI 适配器测试是脚本化 RequestSender，不等于真实 qmi-proxy／UIM 设备验收。

后续仍须：

1. [生产 opener](HOST_IMS_ENDPOINT.md)已实现 MM Ports／sysfs 绑定和代理上的自有 UIM CID；
   真实端点只完成元数据验证，真实 qmi-proxy／UICC 验收仍待进行。
2. 将 opener 接入逐模组业务授权与模式所有者，完善有界退出和不确定状态恢复策略。
3. 自有地址／路由、承载绑定 P-CSCF、小区 PANI 与实际 ESP 注册／媒体，再接模式 UI。
4. 在允许的环境用真实卡验证注册、续期和双向媒体；本次没有 REGISTER、拨号、短信，
   没有更改现用 Asterisk、真实模组状态、默认路由或现有服务。

详细记录：工作区 `publish/vocat-host-ims-sim-20261023/verification.json`。
原 VoCat LICENSE 与地区／设备／SIM 授权校验保持不变，无新增第三方依赖。
QMI 消息布局依据 [libqmi UIM 官方定义](https://github.com/linux-mobile-broadband/libqmi/blob/main/data/qmi-service-uim.json)，
新增事务源码独立编写，不跨许可复制。
