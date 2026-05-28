# CQUPT Rollcall Go

重庆邮电大学自动签到系统 — Go 重写版，增强长时间后台运行稳定性。

## 功能

- **自动定位签到** — 根据课表匹配教学楼坐标，自动提交位置签到
- **自动数字签到** — 检测到有同学已签到成功后，自动提交签到码完成签到
- **扫码/数字签到共享** — 通过 Center 服务器实时广播，一人扫码全体签到
- **课表感知轮询** — 仅在上课时段轮询，课前自动开启
- **Telegram 通知** — 关键事件（签到成功/失败、轮询状态）实时推送
- **指数退避重连** — WebSocket 断线自动恢复，1s → 60s 递增
- **日志去重** — 连续重复的日志自动合并，末尾标注重复次数
- **优雅关闭** — SIGINT/SIGTERM 信号安全退出

## 快速开始

### Docker（推荐）

```bash
# 1. 复制配置
cp .env.example .env
# 编辑 .env 填入你的账号信息

# 2. 启动
docker compose up -d

# 3. 查看日志
docker compose logs -f
```

### 使用 GHCR 镜像

```bash
docker run -d --restart unless-stopped \
  --env-file .env \
  ghcr.io/lulaide/rollcall-go/edge:latest
```

### 本地编译

```bash
go build -o edge ./cmd/edge

# 使用环境变量启动
export EDGE_USERNAME=your_id
export EDGE_PASSWORD=your_pwd
./edge
```

### Kubernetes (Helm)

```bash
# 1. 构建镜像
docker build -f Dockerfile.edge -t your-registry/rollcall-go:latest .
docker push your-registry/rollcall-go:latest

# 2. 创建配置
cat > my-values.yaml << 'EOF'
image:
  registry: your-registry
config:
  edgeUsername: "你的学号"
  edgePassword: "你的密码"
  curriculumApi: "https://cqupt.ishub.top/api/curriculum/学号/curriculum.json"
EOF

# 3. 一键部署
helm upgrade --install rollcall ./deployment/helm \
  -f my-values.yaml \
  --namespace rollcall --create-namespace

# 4. 查看日志
kubectl logs -n rollcall -l app.kubernetes.io/name=rollcall-go -f
```

## 环境变量

| 变量 | 必填 | 说明 |
|------|------|------|
| `EDGE_USERNAME` | 是 | IDS 账号 |
| `EDGE_PASSWORD` | 是 | IDS 密码 |
| `EDGE_CURRICULUM_API` | 否 | 课表 API 地址 |
| `EDGE_CURRICULUM_PRE_MINUTES` | 否 | 课前提前轮询分钟数（默认 10） |
| `EDGE_HTTP_PORT` | 否 | HTTP 端口（留空禁用） |
| `EDGE_CENTER_SERVER_URL` | 否 | Center WebSocket 地址 |
| `EDGE_CENTER_SERVER_SECRET` | 否 | Center 认证密钥 |
| `EDGE_AUTO_LOCATION_CHECKIN` | 否 | 自动定位签到（默认 true） |
| `EDGE_AUTO_NUMBER_CHECKIN` | 否 | 自动数字签到（默认 true） |
| `TG_BOT_TOKEN` | 否 | Telegram Bot Token |
| `TG_CHAT_ID` | 否 | Telegram Chat ID |

## 架构

```
Edge Server                          Center Server
┌──────────────┐    WebSocket     ┌──────────────┐
│  LMS 轮询    │◄──────────────►│  广播签到码   │
│  自动签到    │   rollcall_*    │  状态管理     │
│  HTTP API    │                 │  密钥验证     │
│  TG 通知     │                 │              │
└──────────────┘                 └──────────────┘
```

- **Edge** — 运行在用户侧，轮询 LMS、执行签到、接收共享
- **Center** — 中转站，广播签到码实现多节点共享

## 相比 Python 版的改进

- goroutine + context 生命周期管理
- WebSocket 指数退避重连（1s → 60s）
- 每连接写锁保证并发安全
- panic recovery 防止单点崩溃
- 单二进制部署，内存占用更低
- Telegram 实时通知
- 日志去重，避免重复刷屏

## License

MIT
