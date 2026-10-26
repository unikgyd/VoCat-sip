# 可选 SIP 分机服务：注册、内部通话与消息

设置页新增独立的「SIP 分机服务」卡片。默认关闭，不预置分机、密码、服务器地址或
运营商选线规则。当前已实现 UDP 分机认证注册、可选的[分机文本 MESSAGE](SIP_MESSAGES.md)
及[内部 G.711 通话](SIP_CALLS.md)，后续已接入逐线授权的[模组外呼](CARRIER_OUTGOING.md)
和[模组入呼群振](CARRIER_INCOMING.md)，以及独立授权的[手机短信双向桥](CARRIER_SMS.md)。
外线业务现已按设备选择 VoWiFi 或 [Host IMS](HOST_IMS_APPLICATION.md) 控制器；
显式模式失败不回退 AT 或另一张 SIM。[可选 Asterisk 固定中继](ASTERISK_RELAY_PLAN.md)
第一阶段已实现，合成回环与真实 Asterisk 隔离互通已通过，现网软电话和实卡联合验收未完成。
这不是完整 PBX，模块模式也没有新增 Linux USB 双向音频桥。
它不替代现用 Asterisk，不会因为启用此开关而拨号、发送短信或切换模组模式。

可选的独立 [Asterisk 管理页](../../packaging/asterisk/admin/README.md)已接入 v2 草稿与
默认关闭的受控应用／恢复；PBX 应用不会自动启用这里的网关或导入凭据。
软件与隔离验证见[阶段记录](../../publish/asterisk-web-apply-v2-20261024/README.md)，不是生产联合验收。

## 配置与状态

1. 直连分机添加 2–12 位数字账号，设置 12–128 字节密码。可填写显示名称，不提供默认密码。
   仅使用中继时可不添加本地分机，但须配置并启用合法 peer。
2. 填写本机监听 IP 和端口，例如 `0.0.0.0:5060`；若同机 Asterisk 已使用该端口，请选
   其他端口。IPv6 使用 `[::]:端口` 或具体地址。支持单个 UDP 监听器，不宣称 TCP／TLS。
3. 高级设置默认允许回环、RFC1918 私网和 IPv6 ULA。按部署环境缩小 CIDR 白名单，
   不需要将每个客户端的 Contact 网络地址写入配置。
4. 设置开关后点击「保存 SIP 配置」。下次启动恢复已保存开关和账号；正常退出只关闭
   本地 SIP 套接字与事务，不等待模组或外部服务器。保存变更需客户端重新注册。

绿灯仅表示「分机服务运行中」，不是媒体或运营商网关就绪。卡片显示已注册分机数，
运行时每 15 秒更新状态，也可手动刷新，不覆盖未保存的编辑。未实现的方法返回 405，
`Allow` 默认只宣告 REGISTER 与 OPTIONS，消息与通话分别按独立开关宣告对应方法。
API 的 calls、messages 及 carrier_outgoing／carrier_incoming 跟随对应实现与开关；
carrier_bridge 只表示两方向后端可用，不能以软电话显示“已注册”代替通话验收。
Host IMS／VoWiFi 的 SIM、承载、IMS 和短信状态在逐设备界面单独显示；此 SIP 开关
不会为未就绪线路启动注册、接管承载或修改射频状态。

「可选 Asterisk 中继」默认关闭，使用私网／回环字面 IP:port、独立入站与出站认证、
四方向开关、逐线 ACL 和每线目标；不向 Asterisk REGISTER，也不自动探测。出站预期
Realm 的实际 JSON 字段是 `peer.outbound_auth.realm`。任一电话方向都需出站凭据，
以处理运营商先挂断时的 BYE 挑战。状态 `configured` 仅表示有效配置，不是可达、
已注册或真实通话／短信已验收；后端状态刷新不覆盖未保存的表单与密码。

## 认证与路由边界

默认 Digest SHA-256、qop=auth，可显式选 MD5 兼容旧客户端。nonce 有时限、来源 IP
绑定和 HMAC，验证 nonce-count 防重放；UDP 同事务重传由 SIP 事务层返回缓存响应。
账号必须与 REGISTER 的 From、To 用户部分相同，域名／网段不同不改写分机号，也不
自动加国家码。通话和消息另受全局及逐分机权限控制。
固定 peer 另须精确来源地址与独立 Digest，不继承本地分机身份；第一阶段只接获准
运营商线路，不支持 peer → 本地分机电话／MESSAGE，也不提供未知目标默认中继。

保留 Contact 用于注册响应，但同时记录客户端实际 UDP 来源作为实际投递目的地，
不向 Contact 或 Via 指定的任意地址发请求；注册响应固定发回真实来源。分机注册
每账号最多八个 Contact，支持查询、指定 Contact 注销和通配注销；续期原子更新，
过期自动从查询与状态移除。CSeq 删除墓碑防止旧请求恢复刚注销的绑定。Expires 上限
3600 秒，小于 60 秒的正有效期返回 423；重复、非法参数不静默覆盖。

网络过滤在解析前限制报文大小、网段、来源速率及 UDP peer 数；认证重放和 CSeq
记录也有上限。当前是可信 LAN／VPN 服务，不是公网 SBC，没有 TLS、SRTP、完整
抗攻击或故障切换保证。UDP SIP 未加密，不能直接暴露公网。

## 密码与持久化

`GET /api/settings/sip` 返回配置和状态；`PUT` 接收完整配置，均使用原有管理员会话，
修改还要求 CSRF。密码写入后读取为空，只返回 has_password；留空保留同分机／同
Realm 的密码。修改分机号或 Realm 需要重新填写密码。表单保存失败保留用户输入。

每个本地账号有独立的私有接收代际，不从密码推算。高级项「重置接收身份」不改密码，
但使旧待转发短信不再交给该分机；重新填写密码（即使相同）、删除重建也会重置。
普通保存、停用及重启保留代际。该选项保存一次后清除，不是永久开关。

数据库在 `sip.gateway.private.v1` 中保存 HA1，而非明文密码，整个设置标记 Sensitive。
HA1 等价于认证凭据，仍必须保护数据库和显式含秘密的备份，不是不可逆的安全承诺。
普通公开设置导出隐藏整个条目。API 响应 no-store，审计只记录配置已修改；第三方
SIP 库的原始报文日志不接入可导出的应用日志。

保存先以文件 SQLite CAS 确认原始配置字节与独立 revision，再启动新监听器。
旧 Manager 不能覆盖另一 Manager 的较新配置；端口失败仅在自己仍拥有该候选版本时
回滚并尝试恢复原监听器。提交结果不确定或回滚归属不能确认时停止，不强写旧配置。
启动也先持久确认本地／peer 代际，失败不接收 SIP。恢复会丢失临时注册绑定，客户端
需重新注册。详情见[持久化说明](SIP_SMS_DURABILITY.md)。活动通话中拒绝更改配置；
显式关闭 SIP 服务可以终止通话，普通退出不等待远端应答或模组恢复。

## 开发验证与后续

`go test ./internal/sipgateway` 使用真实回环 UDP 和独立的 icholy/digest 客户端，
不读 SIM、不联系运营商。HTTP 权限回归位于 internal/server/sip_settings_test.go。
前端通过现有 api() 的 snake_case／camelCase 转换，不能用未经转换的接口字段。

内部双腿通话、来显、同账号多终端群振／首接取消、迟到 200 清理、双向 G.711 和
挂断已通过合成终端回环；逐模组模式、业务分派及独立手机短信权限也已接入。
真实软电话、运营商注册／语音／短信、多台实机与故障恢复仍待独立验收，不能以内部
回环或模式 UI 代替完整网关验收。[RFC 4733 按键跨腿](SIP_DTMF.md)已接入；RTCP、
SIP INFO 兼容、保持／重协商等高级能力仍待补齐。

Go 固定 peer、出站单次 Digest、电话／短信 MESSAGE、取消及迟到应答清理已接入，
配置和权限详见 [Asterisk 固定中继第一阶段](ASTERISK_RELAY_PLAN.md)。这不要求
Asterisk 冒充普通数字分机，也不会自动重配现用 PBX。真实 Asterisk 的断网容器互通
使用合成 IMS／PCM，不代表实卡媒体或公网／VPN 通过；最终数字与冻结证据另行留档。

依据：[RFC 3261 注册与事务](https://www.rfc-editor.org/rfc/rfc3261.html)、
[RFC 7616 Digest](https://www.rfc-editor.org/rfc/rfc7616.html)、
[sipgo v1.6.0](https://github.com/emiago/sipgo/tree/v1.6.0)。sipgo 及相关许可保留于 NOTICE
与 LICENSES；VoCat 原始 LICENSE 和授权校验不变。
