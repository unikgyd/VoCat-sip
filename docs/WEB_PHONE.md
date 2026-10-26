# 网页电话

网页电话是已注册 IMS 会话的本机音频端，不是新的注册栈或蜂窝 Host IMS 后端。
登录后从侧栏「网页电话」进入；切换页面不会丢失当前通话。每台模组的状态、
通话和所有权独立，一页同一时刻只接管一通声音，其他模组可由其他网页或 SIP 分机使用。

## 使用

- 使用受信任的 **HTTPS** 或 localhost，允许麦克风；普通内网 HTTP 可管理，不能获取
  浏览器麦克风。[AudioWorklet 的安全上下文要求](https://developer.mozilla.org/en-US/docs/Web/API/AudioWorklet)。
- 选择已注册、空闲的线路，输入 `+` 国家码及完整号码。不会给分机号自动补国家码，
  不会失败后换模组、启用 VoWiFi 或改走 AT。外呼可能产生费用。
- 来电在所有页面显示，按需点击「开启来电提示音」解锁浏览器声音。SIP 分机与网页
  首接互斥，接听后取消其他分机；「拒接整通来电」会结束整个来电，不是仅本机忽略。
- 音频连接状态、麦克风电平、静音与挂断独立显示。电平只证明本机采集，不证明远端听感。
- 关闭页面、退出登录、麦克风处理失败或浏览器暂停音频，会关闭本机音频；服务端随后
  精确清理它接管的通话，不停止模组、不注销整条 IMS，也不恢复／重启基带。
- 后台休眠、锁屏、无网络不能保证提醒；这不是系统推送或 Groundwire 的替代承诺。

## 接口及生命周期

所有 `/api/phone` 接口要求已有登录会话；POST 继续使用原 CSRF 校验，不新增匿名呼叫入口。
`X-VoCat-Phone-Client` 为每页随机 ID，与会话 Cookie 哈希绑定；不得跨用户持久化。

| 接口 | 行为 |
| --- | --- |
| `GET /api/phone` | 配置模组的缓存 IMS 就绪状态、当前通话和音频所有者；不发送 AT／SIM APDU |
| `POST /api/phone/dial` | `device_id, number, operation_id`；只接已验证、已注册且空闲的 IMS |
| `POST /api/phone/answer` | `device_id, call_id`，精确接听仍在振铃的来电，共享 SIP 首接仲裁 |
| `POST /api/phone/hangup` | `device_id, call_id`，管理员显式结束该通话，不能省略 ID 或误挂下一通 |
| `POST /api/phone/dtmf` | `device_id, call_id, digit`；仅当前音频浏览器向原 IMS 会话提交一个协商后的 RTP 按键 |
| `POST /api/phone/hold` | `device_id, call_id, held`；仅原音频浏览器保持／恢复对应 IMS 对话，新增源码待统一验证 |
| `GET /api/phone/media` | `device_id, call_id`；同源认证 WebSocket，使用一次独占音频租约 |

Dial／Answer 返回 `{data:{call,audio_token}}`。音频仅在 `state=active` 且 `media_ready=true`
后接入；183 早期媒体不能冒充已接通音频。音频凭据只在内存和
`Sec-WebSocket-Protocol: vocat-pcm, lease-<token>` 中传送，服务端仅协商公开的 `vocat-pcm`，
不把 token 放 URL、普通状态列表或持久化日志。[WebSocket 子协议](https://developer.mozilla.org/en-US/docs/Web/API/WebSocket/WebSocket)。
原 `/devices/:id/calls/media` 路径也要求租约，不再允许任意已登录页面并行争读 PCM。
其他旧通话控制 API 的 AT 行为未改变；新电话 UI 不调用它们。

服务端在提交 Dial 前沿用私有持久化操作日志，以 operation ID 作键，模组＋号码作指纹。
24 小时内不重放同一操作；响应不确定也不自动生成新 ID 重拨。已知 Call-ID 的迟到返回
会被精确清理；不知道是否提交的情况提示用户核对状态。此日志保证单进程＋单数据库服务
实例内的防重，不能将同一数据库同时交给多个独立 VoCat 进程。

待接通租约最多 90 秒；观察到可用的接通媒体后，15 秒内必须接入。音频掉线给予约
3 秒清理宽限，不自动再拨。WebSocket 每 5 秒 Ping，单次 Ping／下行写入限时 3 秒；
清理 Hangup 使用独立 5 秒上下文，停服务不等待硬件。旧租约不能影响之后的新通话。

## 媒体与验证限制

AudioWorklet 将本机 44.1／48 kHz 单声道采集转换为 8 kHz、16 位小端 PCM，每帧
20 ms；下行限长抖动队列重采样到本机频率。无本地麦克风回放，支持回声消除、静音、
电平；主线程限制积压，服务端限制上行帧长和采样速率。网络差时丢弃陈旧声音，不排队
播放数秒前的语音。该简单窄带重采样不等于专业自适应抖动／语音增强算法。

实际运营商编解码仍由 IMS 提供，目前边界为 8 kHz PCM。默认静态发布不含原生 AMR-NB，
需要按 [AMR_MEDIA](AMR_MEDIA.md) 准备后端；没有新增 AMR-WB 编码、RTCP 或转接。
[本机 IMS 保持／恢复](IMS_CALL_HOLD.md)已接入可选能力和网页入口，新增实现尚未统一验证。
本次已补协商式 RFC 4733 按键，网页只显示对端实际允许的键；取消和不确定结果不会
自动重发。音频断开、通话结束或租约释放会取消在途按键。RTP 接收具备端点核验和
重复结束报告去重；[SIP 分机／peer 的 RFC 4733 跨腿](SIP_DTMF.md)已接线，实际运营商 IVR 未验收。

本轮验证使用合成 IMS、真实 HTTP/WebSocket PCM 及独立浏览器预览；原生 Linux 测试只在
`root@10.0.0.2` 的隔离目录运行，不使用 WSL、不读取实卡、不改 Asterisk、不拨号／发短信。
尚未完成真实麦克风／扬声器、实际软电话首接及运营商通话验收；不要将合成 PCM 当成听感验收。
