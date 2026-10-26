# Linux Host IMS：协议与承载所有者，尚未接入可启用线路

本阶段把现有运营商 SIP 核心从「只能接 SWu 隧道」扩展为可接受独立、经过校验的
蜂窝 IMS 承载。后续已实现[真实 ModemManager 接口的逐模组承载所有者](HOST_IMS_BEARER.md)。
**该所有者尚未接入业务编排、模式切换、API 或开关**，不能据此认为
Linux 已具备 Windows 当前的 Host IMS 实卡能力。原 LICENSE 和地区／设备／SIM
授权检查保持不变。

后续已补入[蜂窝 UIM／ISIM／AKA 事务前置层](HOST_IMS_SIM.md)，包含严格 QMI 协议
适配器、卡内身份、持久挑战防重及清理失败屏障；后续已实现[端点 opener](HOST_IMS_ENDPOINT.md)，
业务接线及实卡 UIM／注册验收仍未完成。

后续已实现[自有内核网络租约](HOST_IMS_NETWORK.md)，真实隔离内核验证双栈地址、
私有策略表、socket 绑定与媒体路由回收。尚未完成生产 P-CSCF 来源证明、SIP／RTP
接线及网络跨重启恢复，不会把该组件标为已建立的蜂窝 IMS 承载。

后续已有[自有 WDS／P-CSCF 来源连接器](HOST_IMS_RUNTIME.md)，补上 USB function／mux、
自有双族 CID、完整承载核对及网络复用时的来源重新验证。真实固件／运营商、MHI／PCIe、
DNS-only、业务模式与实际 SIP／RTP 接线仍未完成。

后续[网络消费者接线](HOST_IMS_DATAPATH.md) 已补实际 SIP socket／默认端口探测绑定、
protected 回复 ESP 覆盖和退出栅栏。MM 小区来源只供缓存诊断，不生成新鲜 PANI；
XFRM 命名空间／多模组隔离、完整媒体、业务编排及实机验收仍未完成。

## 已实现

- `IMSRequest.Cellular` 与 `Tunnel` 互斥，不伪造 ePDG／IKE 已连接状态。
  承载适配器必须证明选定硬件代次、SIM、APN、主机地址、路由和 P-CSCF 来源；
  SIP 层不拥有 PDN，也不在退出时恢复或重启模组。
- 蜂窝 IMS 强制 AKA 与 `ipsec-3gpp`，拒绝配置为普通／可选安全 SIP；VoWiFi
  运营商档案中的可选安全策略不能降低蜂窝模式要求。
- 开始连接、每次 REGISTER（含续期／注销）、AKA 前后，以及新外呼、接听、
  短信和 USSI 提交前只读核验承载。变化即拒绝继续，不自动重建、换卡或切 AT。
  AKA 适配器仍须在自己的事务内锁定同一张卡，不能仅依赖这个外部预检查。
- LTE PANI 来自实际服务小区的 MCC／MNC、双工方式、TAC、28 位 ECI，
  不从归属 IMSI 猜当前位置，不沿用 Wi-Fi PANI；两位和三位 MNC 保持区别。
  REGISTER／鉴权 REGISTER／注销共用该信息，续注册可更新服务小区。
  Wi-Fi／蜂窝成功传输缓存隔离，不互相污染。
- 新 `internal/hostims` WDS 读取器只查询既有进程自有 CID，显式请求 APN、profile、
  地址族和 P-CSCF。补齐 IPv4／IPv6 P-CSCF、域名、MTU 与前缀解析；拒绝重复字段、
  截断、尾随字节、非成功响应和异常长度。profile index 不是 PDP CID。
  生产 QMI 会话提供只读 `IMSRuntime` 能力，但没有把普通数据连接宣布为 IMS 已就绪。
- 端点必须属于精确配置 APN，并有同族本地地址；不把 DNS／网关当 P-CSCF。
  域名只解析结构并保留，尚未实现承载绑定的 DNS 解析，不借系统默认网络发送注册。
- Linux XFRM 安装失败只撤销已获成功确认的添加，遇到现存 SA／策略不整组删除。
  Close 保留失败删除项供重试，成功删除项不会再删；错误不回显可能含密钥的
  iproute2 参数／stderr。初始化回滚有五秒期限。

## 三份实现互补

Go 的蜂窝接入信息与现有 VoSharp LTE PANI 行为对照，共享十条合成格式契约。
QMI 解码核查还复现了 C# 接受不精确 IPv4 字段及重复 APN／地址／P-CSCF 字段的问题，
六条先失败用例验证修复；共享 C# 源镜像重新同步。没有跨许可复制协议源码。
Rust 的逐模组租约／不确定结果／独占资源清理仍作为下一阶段设计参考保留。

## 验证边界

合成 P-CSCF 完成初始 REGISTER → AKA → IPsec 安装接口 → 受保护连接 REGISTER →
200 → 注销；验证了 SIM／地址／P-CSCF 变化及 AKA 前后失效的拒绝路径。
这里使用假 SA 安装器，**不等于已验证该 SIP 流量实际经过 ESP**。

另外在用户指定 `root@10.0.0.2` 的 `unshare -n` 独立网络命名空间中，用真实内核
验证四条 SA／六条策略的安装撤销，以及重复添加失败后保留原有 SA。没有在主机
现用网络命名空间运行 XFRM／TUN 实验；没有改 Asterisk、停止 ModemManager、
改变默认路由、接管真实模组、拨号或发短信。Linux 不使用 WSL。

只读系统清点确认主机有 QMI LTE 模组及 SIM，当时没有 ModemManager bearer。
这不是本程序的实卡 IMS 注册证据；本次没有绕过保留的地区限制。

## 下一阶段必须补齐

1. [逐模组承载所有者](HOST_IMS_BEARER.md)已实现真实 D-Bus 后端、强制 multiplex／显式单 PDN、
   进程锁、SQLite CAS、硬件／卡绑定、迟到响应观察及显式恢复。仍须接入业务编排，
   验证真实 QMAP／NetworkManager 共存及实卡承载；不把 D-Bus 假服务验收当作实卡接管。
2. [UIM／ISIM／AKA 事务层](HOST_IMS_SIM.md)已有合成、race 与原生 Linux 回归；
   [端点发现／绑定与自有 UIM CID opener](HOST_IMS_ENDPOINT.md)已补齐，仍未接业务模式，
   也未在真实 UICC 上验收，不能据此启用实卡模式。
3. [地址／策略路由／socket／媒体租约](HOST_IMS_NETWORK.md)已补齐并通过隔离内核
   验证；后续已有[USB WDS CID／mux 与 P-CSCF 来源连接器](HOST_IMS_RUNTIME.md)，
   仍需真实固件验证、native 端点支持、实际 SIP／RTP 消费者接线、
   接口启动／RA／网络管理器共存、网络跨重启及快速退出恢复、DNS-only P-CSCF。
4. 蜂窝 PANI 的真实小区读取与移动更新、SIP 会话失效及时回收，以及多模组并行隔离。
5. 在合规环境完成真实承载、ESP 报文、自动续期、入呼／外呼、短信和双向音频验收，
   然后才接入线路模式 UI。USB 语音和 Asterisk 协议中继仍是独立缺口。

执行结果见工作区 `publish/vocat-host-ims-foundation-20261023/verification.json`。

协议布局依据：[libqmi WDS 消息定义](https://github.com/linux-mobile-broadband/libqmi/blob/main/data/qmi-service-wds.json)，
以及 Linux Foundation Gobi 原始结构 `sWDSGetCurrentSettingsResponse_PCSCFIPv6AddressList`
（[公开源码存档](https://github.com/E3V3A/libqmi/blob/master/gobi-api/GobiAPI_2013-07-31-1347/GobiConnectionMgmt/GobiConnectionMgmtAPIStructs.h)）。
