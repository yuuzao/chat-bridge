# Chat Bridge

单进程 HTTP 中转。本机没有公网地址时，把一次查询交给这台服务；远端客户端异步回传文本或一个文件，发起方再取回。

任务写入 SQLite，进程重启后未取走的内容仍然在。

## 特性

- 文本和单个文件走同一笔任务
- 远端用回调交回结果
- 任务全量落库（SQLite），重启不丢
- 查询和下载都是即时的：客户端短轮询 `GET /api/tasks/{id}`，不再有长轮询 `wait` 参数
- 读取、下载都不删除任务，可以任意次幂等重复拉取
- 只用 Go 的纯 Go SQLite 驱动，镜像仍是 `CGO_ENABLED=0` + `FROM scratch`

## 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/tasks` | 校验后写入 `PENDING` 任务，返回 `202`，随后异步转发到 Bot |
| POST | `/api/webhook` | 把 `PENDING` 任务原子更新为 `DONE`，重复回调不覆写，返回 `200`；任务不存在返回 `404` |
| GET | `/api/tasks/{id}` | 立即返回当前状态；带 `wait` 参数返回 `400` |
| GET | `/api/tasks/{id}/file` | 立即返回原始文件字节；带 `wait` 参数返回 `400` |

四个接口都要认证：请求头 `X-Auth-Token`，或在 JSON/表单字段里放 `auth_token`。

## 配置

配置只有一个 YAML 文件，字段含义、必填性写在 `config.example.yaml` 的注释里，程序不提供默认值。

```yaml
listen_addr: "127.0.0.1:8080"
auth_token: "replace-me"
bot_url: "http://127.0.0.1:9/bot"
bot_bearer_token: "replace-me"
max_body_bytes: 10485760
db_path: "/data/chat-bridge.db"
forward_timeout_seconds: 30
```

`db_path` 指向 SQLite 数据库文件。父目录会被自动创建；同目录还会有 `-wal`、`-shm` 文件，所以不要用单文件只读挂载。
