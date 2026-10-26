# 短信与验证码只读 API（Go Linux）

此功能为可选项，默认关闭。VoCat 运行时，可用独立 Bearer Token 查询数据库中已保存的
收件短信和验证码；不访问模组、不拨号、不发送或删除短信，也不修改已读状态。
这不代替 SIP MESSAGE 转发，不依赖 Asterisk，不授予管理页面权限。

## 启用和停用

进入“设置 → 短信与验证码 API”，保留默认 `127.0.0.1:8787` 或填写具体的本机私网 IP，
生成或填写独立 Token，打开开关，再点击“保存并应用 API”。生成 Token 或切换开关都
只是编辑草稿，保存后才生效。生成的新 Token 请先复制保存，应用后不再显示。

Token 必须是 32–128 位 ASCII 字母、数字、下划线或连字符；生成器使用 32 字节随机数。
保存新 Token 后，旧 Token 失效；留空表示保留已配置的 Token。数据库只保存 SHA-256 摘要，
不保存或返回明文。该配置独立于管理员密码、SIP 和通知机器人，标记为敏感设置。
SQLite 文件沿用应用的 `0600` 权限；有权限读取数据库的用户仍可直接读取短信，须保护数据目录和备份。

保存失败不改变原监听器和 Token。保存成功但 IP／端口无法监听时，显示错误状态，
接口保持停止；修正后重新保存即可重试。启用状态在重启后恢复；关闭并保存后释放监听端口。
配置损坏时保持关闭，不自动改成通配监听或使用其他凭据。退出时立即关闭接口并取消查询，
不等待模组恢复。不会自动配置防火墙、代理或端口映射。

## 查询规范

路径、查询参数、返回字段与 [Windows API](../../docs/SMS_API.md) 对齐：

```http
GET /api/v1/sms?keyword=login&limit=20
Authorization: Bearer <独立Token>
```

```http
GET /api/v1/otp?maxAgeSeconds=300
Authorization: Bearer <独立Token>
```

支持 `keyword`、`sender`、`slotId`、`maxAgeSeconds`、`since`、`limit`、`otpOnly`，
只接受 GET，重复、未知或无效参数返回 400。`+`、中文、带时区时间须正确 URL 编码。
关键词为忽略大小写的原文包含匹配，`%` 和 `_` 没有 SQL 通配含义。
时间以短信已保存的时间为准，最多最近七天，按时间和行 ID 倒序；未来短信不返回。

`slotId` 是短信保存时的 `device_id`，不会根据当前模组或换卡后的配置重新归属。
不指定时查询所有线路；这是筛选条件，**不是授权边界**。持有 Token 可读取全部线路收件箱。

列表返回 `items`、`count`、`scanLimited`、`generatedAtUtc`；每条仅含
`id`、`slotId`、`sender`、`text`、`timestampUtc`、`code`、`textTruncated`。
不返回 IMSI、ICCID、原始 PDU 或设备配置。正文按最多 4096 UTF-16 单元截断，不切开 emoji；
提取验证码使用截断前正文。无匹配列表为 `[]`；验证码为 `found=false`、`code=null`、`message=null`。
查询不消费验证码。验证码仅是启发式识别，不能作为身份验证已经成功的证明。

Go 端采用上下文启发式提取，支持全角与分隔数字，并排除紧邻的订单号、金额等；
与 Windows／Rust 共享 21 条验证码样例、32 条查询边界和 11 条 Unicode 匹配样例。
共享样例通过不代表任意文本的评分细节完全一致，疑似误判应连同合成／脱敏样例反馈。

OTP 查询最多检查 1000 条符合其他条件的候选；触顶且尚未取足结果时
`scanLimited=true`，请缩小时间、关键词或线路范围。查询约五秒超时，最多并发八个。
单次监听器全局每分钟最多 120 次请求，鉴权失败也计数；429 返回 `Retry-After`。
存储错误、超时或并发耗尽返回脱敏的 503；不回显底层 SQL、路径或短信。

## 网络和安全

仅允许具体回环 IP 或 RFC1918／CGNAT／IPv6 ULA 私网地址；非回环须显式勾选允许 LAN／VPN。
不允许 `0.0.0.0`、`::`、DNS 主机名、链路本地或公网 IP。端口范围 1024–65535。
请求同时核对实际本地 socket、实际私网来源和与监听地址一致的 Host；不信任
`X-Forwarded-For`。无 CORS，包含 Origin 的请求一律拒绝，包括空 Origin。

需要跨网访问时，使用可信 VPN 或自行部署 HTTPS 反向代理，不要直接映射明文 HTTP 到公网。
代理须保留 `Authorization`、为上游设置与监听 IP／端口一致的 Host、关闭缓存，并避免记录
Token、完整查询 URL、关键词和短信正文。接口不记录这些内容，但第三方代理日志需自行保护。
响应带 `Cache-Control: no-store, private` 和 `X-Content-Type-Options: nosniff`。

管理员设置路由是 `/api/settings/sms-api`，仍需管理会话，写入额外要求 CSRF；
短信 Token 不能访问它。反过来，管理员 Cookie 也不能代替只读接口的 Bearer Token。
面向网页集成时通过受信任后端调用，不把这个共享 Token 下发到浏览器。

本阶段只做合成数据、隔离端口、SQLite、真实本地 HTTP 和原生 Linux 软件验证；
没有启用现用实例、读取真实收件箱、操作 SIM 或修改生产 Asterisk。
