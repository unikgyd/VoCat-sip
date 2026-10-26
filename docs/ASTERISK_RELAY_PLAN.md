# Go 可选 Asterisk 固定中继：第一阶段实现与后续边界

2026-10-24：**固定 peer 第一阶段已实现，合成回环及真实 Asterisk 隔离互通已通过；
现网软电话和实卡联合验收尚未完成。** 保留原文档路径，不再将本功能写成纯设计，也不将
本文当作现用服务器的部署说明。现有[本地 SIP 服务](SIP_GATEWAY.md)、
[逐线 IMS 业务](HOST_IMS_APPLICATION.md)与[手机短信桥](CARRIER_SMS.md)现已接入
独立固定中继；未接入现用 Asterisk 的 SIP，未重配或重启生产容器、模组或服务。
真实 Asterisk 验证只使用下述自建断网测试容器。

同一 Go 应用保留本地分机 Registrar，并可选择与一个管理员配置的 Asterisk
固定 peer 双向交换电话和 MESSAGE。中继默认关闭，不预置地址、账号、密码、国家码、
号码或线路；不开启它不影响本地分机、网页电话或独立模组线路。原 LICENSE、授权校验
及收费业务限制保持不变，不跨许可复制 Windows／Rust 实现。

## 第一阶段已实现范围

- 一条可信 LAN／VPN 内的 UDP 固定 `IP:port`，独立于本地分机注册表。端点须为
  回环／私网字面 IP、位于 `allowed_cidrs` 内、与监听器同地址族且非本机监听自环；
  不接受域名、SIP URI、IPv4-mapped IPv6 或 zone。不伪造 REGISTER 或“已注册”状态。
- 不要求 Go 向 Asterisk REGISTER；不自动安装、重启 Docker 或重写 `pjsip.conf`／
  dialplan。后续配置导出也只能输出管理员审阅的模板，不能冒充已应用。
- 支持 Asterisk → 获准 IMS 线路的电话／手机短信，以及获准线路 → Asterisk 的
  电话／短信 MESSAGE；四个方向分别授权。所选 Host IMS／VoWiFi 未就绪就失败，
  不启动注册、不改模式、不换卡、不经 AT 回退。
- 本地分机之间的电话／MESSAGE 保持原行为。第一阶段不增加任意外域代理或
  “找不到分机就交给 Asterisk”的默认路由。本地分机 → Asterisk 远端分机若后续需要，
  应单独增加显式前缀／目标空间和获准账号列表。
- **peer → 本地分机电话和 MESSAGE 当前明确拒绝**，不能继承本地分机权限，也不把
  已知本地账号改成 SIM 手机号。peer 凭据不能 REGISTER；REGISTER 仍使用原本的
  本地账号规则，即使来源恰好与 peer 相同，也不将中继身份注册成分机。
- 已补 [RFC 4733 DTMF 跨腿](SIP_DTMF.md)；不宣称 SIP INFO 兼容、公网 SBC、NAT 穿透、
  TCP／TLS、SRTP、RTCP、转接、保持或 re-INVITE。
  这些不应被一个“中继已启用”绿灯隐含承诺。

## 实际配置与身份契约

`sipgateway.Config` 已有可空的 `peer`，不复用 `accounts` 或 `registrations`。
第一阶段只支持一个固定 peer；通过原管理员会话／CSRF 保护的
`GET`／`PUT /api/settings/sip` 读写，界面位于「SIP 分机服务 → 可选 Asterisk 中继」。

| `peer` 内实际 JSON 字段 | 契约 |
| --- | --- |
| `enabled`、`endpoint` | 默认关闭；精确固定 IP、端口、UDP，拒绝未指定／组播地址、本机 SIP 监听器和明显自环 |
| `inbound_auth` | 独立用户名、写入式密码、`has_password`；使用本地 Realm 验证 Asterisk 入站请求 |
| `outbound_auth` | `username`、**`realm`（不是 `expected_realm`）**、写入式 `password`、`has_password`、`algorithms`；界面称「出站预期 Realm」，只向精确 peer 的匹配挑战提供凭据 |
| `outbound_auth.algorithms` | 默认 `["SHA-256"]`；只支持 SHA-256 与显式配置的 MD5，不自动降级到未获准算法 |
| `allow_calls_from_peer`、`outgoing_lines` | 默认无权外呼；仍须全局通话、外线开关及所选线路允许外呼 |
| `allow_sms_from_peer`、`sms_send_lines` | 默认无权发送手机短信；仍须手机短信开关、线路发送权限、限额及原卡校验 |
| `allow_calls_to_peer`、`allow_sms_to_peer` | 默认无权向该 peer 转交模组入呼／短信；不得由接入地址自动推导 |
| `incoming_routes` | 显式 `device_id` → `call_target`／`sms_target`；空目标就是不转发，并受线路原有入呼／收短信开关约束 |

字段已进入长度、数量、唯一性、目标 user 和线路引用校验。认证子字段为 `username`、
`password`、`has_password`；`incoming_routes` 每项为 `device_id`、`call_target`、
`sms_target`。目标只填受限 user，不填完整 SIP URI。每设备一条 route，电话与短信
目标可不同；对应目标空即不转发。全局 SIP 服务可以没有本地账号，仅使用合法启用的
peer；不需要伪造数字分机。默认空白、关闭的 peer 对象也可保存。

入站须同时满足网段白名单、精确来源 `IP:port` 和独立 Digest；IP 匹配不是认证。
本地账号仍按原规则要求 From 用户与认证用户一致。peer 使用单独的 principal 类型，
不能靠取消全局 From 检查接入。鉴权身份、逐线授权身份与展示来显分离；From 或
`P-Asserted-Identity` 不授予线路权限。第一阶段没有信任任意 peer 原始来显的开关。

`allow_calls_from_peer` 或 `allow_sms_from_peer` 要求入站凭据。任一电话方向以及
`allow_sms_to_peer` 都要求出站凭据：即使只允许 Asterisk 外呼，运营商先挂断时
发给 peer 的 BYE 也可能被挑战；这不额外授权新的出站 INVITE。仅 SMS-from-peer
可以没有未使用的出站凭据。

已分别保存入站本 Realm 和出站 `outbound_auth.realm` 的 MD5／SHA-256 HA1，
不复用本地分机摘要，读取不返回密码或摘要。
变更用户名／Realm 必须重新输入相应密码，空密码只保留同一凭据身份。出站挑战
Realm、来源或算法不符即拒绝，不能用本地凭据试探其他 Realm。SHA-256 优先，MD5
只能显式兼容；Digest 算法依据 [RFC 8760](https://www.rfc-editor.org/rfc/rfc8760.html)。
HA1 仍是可用凭据，须继续 Sensitive、no-store、脱敏导出，不宣称磁盘加密。

## 已接线的位置与本地注册共存

| 现有位置 | 已实现的职责 |
| --- | --- |
| `internal/sipgateway/config.go`、`carrier_policy.go` | peer 校验、深拷贝、独立秘密版本／迁移与四方向权限；旧配置无 peer 时行为不变 |
| `runtime.go` 的 `authorize`／`register` | 区分本地账号与 peer principal，源地址与凭据双门槛；peer 身份不接受 REGISTER，本地数字账号继续正常注册 |
| `calls.go`／`carrierRoute` | peer 请求只走获准外线，peer 到本地账号拒绝；本地账号互拨仍优先，不因离线转外线或 peer |
| `carrier_incoming.go` 的 `offerIncoming` | 将显式 peer 目标加入原群振／首接仲裁，保留本地分机与网页共同响铃、首接取消和迟到接听清理 |
| `peer_call_branch.go`、`call_wire.go` | 固定目标、逐 attempt 处理和单次出站 Digest，不仅给 `client.Do` 加凭据 |
| `peer.go`、`messages.go` | peer MESSAGE 的独立鉴权与外线短信权限；不开放 peer 到本地聊天 |
| `sms.go` 的 `QueueSMS`／`DeliverSMSOnce`／`deliverInbox` | 独立 peer 收件人种类、投递与私有持久化身份，不把固定 peer 塞进注册表或假装在线分机 |
| `internal/server/sip_settings.go` | 沿用管理员会话、CSRF、no-store 和 `Manager.Apply`；API 不可旁路配置校验或直接写监听器 |
| `web/src/components/settings/SipGatewayCard.tsx` | 扩展 Config／Form、加载和完整 PUT、验证与状态，不让旧表单保存时擦掉 peer 字段 |

已使用明确区分“已观察 REGISTER 来源”和“管理员固定 peer”的投递目标类型，供
内部通话、模组群振、MESSAGE、短信队列共用。网络目的地保持各自的来源证明；
不能让 Contact、Via、Route、响应中的 URI 或原始 From 覆盖固定端点，也不能用固定
端点绕过现有完整响应身份校验。固定 peer 的凭据／目标与本地账号不能互相抢占。
peer principal 不套用“一个数字分机同时一通”的账号忙线键；并发按原 IMS 线路、
呼叫与媒体所有者隔离，同一中继可使用不同模组的独立通话租约，仍受全局容量上限约束。

## 选线、防环与来显

认证成功不等于任意拨号权。peer → 运营商仍调用现有规范化／选线逻辑，权限取
peer allowlist、线路开关、全局开关、设备策略与运行时证据的交集；默认线路不赋权。
只对明确配置的国内完整位数补国家码，短号和本地分机不补。选中线路不可用不换线路。
peer → 本地分机当前不支持；未来若开放，须另有目标白名单，不能继承运营商发送权限。

处理路径区分 peer／本地账号身份，保留原操作及线路所有者；同一 peer 进入的业务
不直接从同一 peer 反射。配置拒绝本机监听自环；运行时仍检查事务
重放、Max-Forwards 与来源，不能只靠配置排除所有网络自环。不跟随 3xx 自动改目的地，
不按未知 URI 域名转发，不把 Asterisk 未知分机默认交给 SIM。

正常呼叫／消息展示可统一身份域，但身份域不决定 UDP 投递。Asterisk dialplan 需把
外线与内部路由放进明确 context，并避免回送 VoCat；官方的 endpoint、aor、identify、
`auth`／`outbound_auth` 是不同配置角色，不能用一个对象替代所有授权。
参见 [Asterisk PJSIP 配置关系](https://docs.asterisk.org/Configuration/Channel-Drivers/SIP/Configuring-res_pjsip/PJSIP-Configuration-Sections-and-Relationships/)
与[认证说明](https://docs.asterisk.org/Configuration/Channel-Drivers/SIP/Configuring-res_pjsip/PJSIP-Authentication/)。
本文不生成针对现用服务器的具体配置。

## 电话事务、取消与媒体

peer 已有独立 `peer_call_branch.go`，维护逐 attempt／dialog 序号与清理状态，
不是在失败时新建一通业务呼叫。实现契约是：

1. 只处理通过完整来源／事务校验的 401／407；INVITE 的非 2xx 最终响应先按原事务
   ACK，再用新 Via 分支及递增 CSeq 进行最多一次凭据重试。保留旧 challenge 事务，
   能处理其重传；已经取消或超时，不得被挑战重新启动呼叫。
2. CANCEL 指向当前实际 INVITE attempt；ACK 与 CANCEL 不盲目套普通鉴权重试。
   每个 attempt／To-tag 的迟到 2xx 都准确 ACK 并清理 BYE，不能让旧 attempt 抢到
   媒体或导致另一线路接听。所有重试共用原振铃／清理 deadline。
3. BYE 的单次 peer 鉴权保留原总预算；不因 401／407 重置超时。双向 BYE、异常断开、
   服务关闭和首接仲裁都结束同一原始 IMS 呼叫及其媒体，不按“当前设备模式”重新找后端。
4. 复用 source、Call-ID、From／To、tags、CSeq／method、Via 的精确校验；配置热改不能
   把现有 dialog 改发新 peer。活动通话拒绝改配置，显式关闭只清理本服务拥有的资源。

ACK、CANCEL、dialog 和鉴权重发的标准依据见
[RFC 3261 §9、§12、§17、§22](https://www.rfc-editor.org/rfc/rfc3261.html)。
“最多一次重试、固定端点和不延长预算”是本项目的收敛策略，不是通用 SIP 功能全集。

媒体先保持现有 G.711 SIP 侧桥接和每腿独立所有者；不能以收到 200 或 RTP 包计数
代替可听的双向语音。当前媒体使用临时 UDP 端口、信令来源 IP 约束与端口学习，
尚没有可配置公网 SDP 地址／RTP 端口范围。第一阶段要求 peer 的可达信令及媒体地址
满足现有模型；不同媒体主机／NAT／公网映射须另行设计受限媒体地址策略，不能关闭
源地址检查解决。只有 SIP 端口映射不构成可用 RTP 路径。

## MESSAGE、手机短信与持久化

peer 文本消息与收费手机短信必须显式区分。既不把所有 MESSAGE 当 SMS，也不在内部
消息失败后改发手机短信。获准的手机短信继续使用逐线配额、原 IMSI＋ICCID 校验和
持久化收费操作日志；固定 peer 的认证身份加入稳定操作身份，展示号码不能替代它。

出站 MESSAGE 可在严格匹配的挑战后单次重试，使用新事务但仍是同一逻辑 outbox 项；
网络结果未知不新建业务操作重投。保留来源及全套响应身份验证、UTF-8 `text/plain`
和整包 UDP 上限，不截断短信冒充成功。MESSAGE 的方法与 UDP 限制依据
[RFC 3428](https://www.rfc-editor.org/rfc/rfc3428.html)；本阶段不增加 CPIM 或分段协议。

短信收件队列已保存独立 `peer` recipient 种类，身份绑定固定端点、出站 Realm、目标
user 和凭据代次。重启可保留尚未交给传输的队列项；换 peer、Realm 或重设凭据后
不能继承旧私密待投短信。旧本地分机项不会被解释成 peer 项。peer 不等待 REGISTER
或在线探测；尝试投递前先持久化未知结果。
无明确投递结果仍保留未知状态，不能因重新上线／重启自动补发不确定项。

保留 `sms.<线路>.<原SIM哈希>.<号码>` 回复身份；Asterisk 消息路由必须保留可回传的
原卡回复目标，不把它改成默认分机／默认 SIM。显示发件号码和路由回复 user 分离，
跨 LAN／VPN 身份域变化不更换原 SIM 或重置防重身份。

## UI／UX 和生命周期

设置卡新增折叠的「可选 Asterisk 中继」区域，默认关闭；基础只展示开关、固定端点、
独立认证和四方向／逐线权限，其他策略折叠。密码写入后清空，保留 `has_password`；
加载旧配置时明确默认，新增字段进入深拷贝和完整保存，不能因普通分机编辑丢失中继。
本地分机重命名／删除及线路删除须同步校验引用，不暗自转给其他目标。

状态直接使用后端 `status.peer` 的 `disabled`／`configured`／`unavailable`，带
`enabled` 和 `endpoint`；`capabilities.fixed_peer` 只是能力标记。`configured`
只表示运行监听器持有有效配置，**不是 reachable、已注册、认证或通话／短信已验收**。
刷新状态不覆盖未保存表单／密码，也不读取旧草稿作为当前事实。第一阶段没有自动
OPTIONS、REGISTER 或其他连通探测，不因配置保存而拨号、发短信或启动 IMS。

沿用 `Manager.Apply` 的准备、持久化、监听失败回滚和活动通话限制；旧配置无 peer
继续原行为。关闭／替换 peer 取消本服务拥有的事务、媒体和未提交任务，不等待模组
恢复。已经提交而结果未知的短信继续保留防重事实；不能随监听器换代清除正在执行的
逐模组隔离锁，或释放尚未清理 ACK 的 Host 所有者。

## 离线验证与后续验收

第一阶段配置／秘密、API／UI、独立权限、双向电话和短信 MESSAGE、原卡回复及
持久化 peer recipient 已接线。合成回环及断网容器内的 Asterisk 22.5.2 互通已通过：
SHA-256／MD5、双向 INVITE／MESSAGE、PCMA 回声、双向 BYE 和 Asterisk 发起的 CANCEL。
后者使用合成 IMS，
不是运营商或手机通话。最终测试数字与源码冻结证据在
工作区根目录的 `publish/vocat-asterisk-peer-20261024/` 单独留档。实际软电话／实卡、真实多模组及
公网／VPN／NAT 拓扑仍须后续独立验收。

每阶段先用私有回环 UDP 假 peer 和合成 IMS 后端验证；Linux 只在用户指定
`root@10.0.0.2` 的隔离目录运行，不用 WSL。不接真实模组、不发真实 REGISTER／
电话／短信、不改现用 Asterisk／路由。未来真实测试需单独标注测试对象、授权和结果。

离线验证围绕：默认无权、旧配置迁移、凭据不泄漏／错误 Realm、不匹配源 IP／端口、
伪造响应／Contact／来显、已知离线分机不转外线、选线与限额、错误卡／不就绪不回退；
本地与 peer 并存、入呼群振／首接取消、挑战时取消、迟到 2xx、BYE 鉴权总预算、
双向可比对 PCM、单线故障不影响其他线；短信未知不重投、换卡回复拒绝、peer 凭据／
端点轮换不能接旧私信、队列重启、关闭与配置回滚。合成回环通过不能记为实网通过。

Windows C# 已有固定 peer、出站 Digest 和双向 MESSAGE 参考；Rust 参考的固定 peer
仅覆盖 INVITE，其 peer MESSAGE 仍拒绝。Go 独立实现并使用合成协议契约，不能以
另一端已有功能证明本实现可用，也不能为接线移除原项目许可或授权边界。
