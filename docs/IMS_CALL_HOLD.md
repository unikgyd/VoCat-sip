# IMS 通话保持与恢复

本次实现已纳入[统一软件验收](VERIFICATION_20261026.md)，包括精确对话 re-INVITE／ACK、
保持／恢复与拒绝后保留原通话；真实软电话和运营商互通仍须实机验收。

`POST /api/phone/hold` 接收 `device_id`、精确 `call_id` 和显式布尔 `held`。
管理登录、CSRF 和当前浏览器的音频所有权沿用网页电话边界；其他浏览器、SIP 分机持有的
通话及 USB 模块语音不通过此入口操作。业务经原通话控制器分派，不受之后默认线路切换影响。

## 状态和媒体

IMS 活跃通话通过可选 `CallHoldController` 提供能力。`can_hold` 来自实际 IMS 对话／媒体，
`held` 表示本机已确认的保持状态，`hold_pending` 表示变更在途。通话继续为 `active`，
原 Call-ID、PCM、WebSocket 和清理租约保留；保持不释放媒体给另一消费者。

保持 offer 使用 `sendonly`，恢复使用 `sendrecv`。SDP origin ID 在同一媒体会话内稳定，
version 随生成递增。answer 需匹配已提供的载荷与方向；接入未提供的格式不算成功。
媒体按本机保持和对端方向控制收发，保持期间丢弃采集和旧下行，不在恢复后重放声音或按键。

网页只对实际具备能力且由本浏览器接管的通话显示保持／恢复按钮。挂断可以取消在途变更；
保持结果未确认时不会显示为已成功，也不自动重试。Host IMS 的内存通话快照在业务事务期间
仍可读取，避免状态轮询把在途保持误判为原通话消失。

## 信令与并发

使用原 Call-ID／标签／Route、独立递增 CSeq 和新事务 branch 发起 re-INVITE。
与另一 offer 或按键事务冲突时返回忙；本机 offer 在途收到对端新 offer 时返回并缓存 491。
只对匹配原事务及对话的最终回复进行提交。2xx 使用对话 ACK，拒绝使用原 INVITE 事务 ACK；
普通拒绝保留先前状态，不自动重新发起 offer。

在途变更限时 15 秒。取消、超时、写入未知或已接受但媒体提交失败时结束该精确对话，
不继续使用未知状态。事务回复通道保留 32 秒以处理重复／迟到最终回复，必要时 ACK 后
精确 BYE 清理；保留数量按通话限额，不无限增长。没有借用下一通电话或另一个模组。

本实现用于本机 IMS 保持／恢复，不新增 SIP B2BUA 对端 re-INVITE 转接或会议。
协议依据：[RFC 3264 §8.4](https://www.rfc-editor.org/rfc/rfc3264.html#section-8.4)、
[RFC 3261 §14](https://www.rfc-editor.org/rfc/rfc3261.html#section-14)。
