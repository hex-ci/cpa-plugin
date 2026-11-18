# Changelog

## 1.1.2

### 修复

- **模型容量队列（10605）不再被当成硬错误**：上游把 10605 放在 HTTP 200 的 SSE 帧里
  （`statusCodeValue: 403`），插件原先只看传输状态码，`status >= 400` 判断直接跳过，
  队列检测分支从未执行过，客户端拿到的是 500 `internal_server_error`。现在打开流后先读到
  第一帧结束再判定，排队请求会按 `retryAfterSeconds` 等待后重试，等待预算耗尽才报错。
- **队列超时以 429 返回并带 `Retry-After`**：原先插件把错误对象塞在 stream 的 payload 里，
  宿主会把它当正常 chunk 转发，客户端收到 200 + 错误正文，宿主也无法按 429 换凭据。
  现在走 envelope 的 `error` 字段（异步流走 `error_detail`），状态码、`retry_after_seconds`
  与 `credential_scoped` 都能传达到宿主，一个账号排队时其余账号可继续服务。
- **流式请求先同步打开上游再返回**：原先先返回 200 再起 goroutine 连接上游，队列、限额、
  凭据失败都只能作为流内错误帧，永远拿不到正确状态码。现在打开与失败分类都发生在响应之前，
  流式与非流式对失败的分类一致。
- **修复流式响应被截断**：读取首帧用于队列判定时，缓冲会预读后续字节，原先只归还已读部分，
  剩余内容丢失，客户端报 `upstream stream closed before any chunk carried finish_reason`。
  现在全部预读字节都会归还，流式正文完整。

### 其它

- 版本号 1.1.1 → 1.1.2。
