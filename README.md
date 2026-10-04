# SMTP 联调收信后端

这是一个仅用于研发联调的 SMTP 收信与 HTTP 查询服务。服务只接收邮件，不解析 MIME、不修改原文、不向外转发。

## 功能

- SMTP 与 HTTP 均只绑定回环地址，端口可配置。
- 支持 `HELO`、`EHLO`、`MAIL FROM`、`RCPT TO`、`DATA`、`RSET`、`NOOP`、`QUIT`。
- 严格要求 CRLF，使用逐字节读取处理 TCP 分包和命令管道。
- 只接受配置中的收件地址；域名大小写不敏感，本地部分大小写敏感。
- 允许空发件人 `MAIL FROM:<>`；重复信封收件人只保存一次；不读取或推断邮件 `To` 头。
- DATA 支持点透明：仅去掉行首转义点，保留其余字节和 CRLF；半封断连不落库。
- SQLite 在一个事务中保存邮件、信封和收件人关联，成功提交后才返回 SMTP `250`。
- HTTP 可按收件人列元信息、按 ID 查看信封、下载原始 `.eml`。
- 限制连接数、读取期限、命令行长、收件人数和邮件字节数；超限关闭对应连接。

## 配置

通过环境变量配置：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SMTP_ADDR` | `127.0.0.1:8025` | SMTP 监听地址，只允许 `127.0.0.1`、`localhost` 或 `::1`。 |
| `HTTP_ADDR` | `127.0.0.1:8080` | HTTP 监听地址，只允许回环主机。 |
| `DB_PATH` | `mail.db` | SQLite 数据库路径。 |
| `ALLOWED_RECIPIENTS` | 无，必填 | 逗号分隔的允许收件地址，例如 `dev@example.com,qa@example.com`。 |
| `MAX_CONNECTIONS` | `50` | 每个服务的最大 TCP 连接数，超出立即拒绝并关闭。 |
| `READ_TIMEOUT_SECONDS` | `300` | 每次读操作的期限。 |
| `MAX_LINE_BYTES` | `2048` | SMTP 命令或 DATA 单行最大字节数（含 CRLF）。 |
| `MAX_RECIPIENTS` | `100` | 单封邮件最大不同收件人数。 |
| `MAX_MESSAGE_BYTES` | `26214400` | 单封邮件原文最大字节数，不含 DATA 终止点行。 |

地址本地部分必须是 ASCII dot-atom；域名由点分标签组成。重新问候或 `RSET` 会清空未完成信封；一封完成后自动清空信封，可继续下一事务。

## 运行

依赖已随仓库使用 Go modules vendor 管理，需要启用 CGO 以编译 `go-sqlite3`：

```sh
export CGO_ENABLED=1
export ALLOWED_RECIPIENTS='dev@example.com,qa@example.com'
export SMTP_ADDR='127.0.0.1:8025'
export HTTP_ADDR='127.0.0.1:8080'
go run -mod=vendor .
```

构建：

```sh
go build -mod=vendor -o bin/smtp-capture .
```

测试：

```sh
go test -mod=vendor ./...
go vet -mod=vendor ./...
```

## SMTP 示例

以下示例使用系统 `swaks`：

```sh
swaks --server 127.0.0.1:8025 \
  --from '<>' \
  --to dev@example.com \
  --add-header 'X-Test: 1' \
  --body 'hello capture'
```

也可以用原始 TCP 发送，便于确认严格 CRLF 和点透明：

```sh
printf '%s\r\n' \
  'EHLO example.com' \
  'MAIL FROM:<sender@example.net>' \
  'RCPT TO:<dev@example.com>' \
  'DATA' \
  'Subject: raw test' \
  '' \
  '.dot escape line' \
  '.' \
  'QUIT' | nc 127.0.0.1 8025
```

## HTTP 接口

列出某收件人的邮件：

```sh
curl -sG 'http://127.0.0.1:8080/messages' \
  --data-urlencode 'recipient=dev@example.com'
```

响应中的字段：

- `id`：唯一邮件 ID。
- `received_at`：UTC RFC3339 接收时刻。
- `sender`：SMTP 信封发件人；空发件人为空字符串。
- `recipients`：去重后的 SMTP 信封收件人，按提交顺序排列。
- `size`：原文字节数。

查看信封：

```sh
curl -s 'http://127.0.0.1:8080/messages/<id>'
```

下载原始邮件：

```sh
curl -fL 'http://127.0.0.1:8080/messages/<id>/raw' -o message.eml
```

非法或重复查询参数、非法地址格式返回 `400`；未配置的收件人或不存在的 ID 返回 `404`。

## 存储

SQLite 表：

- `messages(id, received_at, sender, raw)`
- `message_recipients(message_id, recipient)`

写入采用 `BEGIN` / `INSERT` / `COMMIT`。如果任一步失败，事务回滚，不会留下邮件记录或部分收件人关联。服务启用 WAL，正常关闭后重启可继续查询。
