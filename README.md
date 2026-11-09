# dlog · Docker 本地日志查看器

用 Go 写的单文件日志查看器，**直接读 Docker 写在磁盘上的容器日志文件**，浏览器里实时跟随。

和 Dozzle / Plume 这类工具的区别：它不代理 `docker logs`，而是自己解析 `json-file` 的 NDJSON。
代价是只读、不能 exec，好处是**不依赖 docker 守护进程**——挂不挂 socket 都能跑，权限面小得多。

- 零第三方依赖，纯标准库
- 二进制内嵌前端，单个静态 ELF（`CGO_ENABLED=0`），scp 到服务器就能跑
- 自动从 `daemon.json` / `docker info` / 默认路径三级定位 data-root
- 可选访问口令（`-web-token`），防止把日志页面裸奔在公网上
- 容器名旁边显示实时 CPU / 内存占用，口径与 `docker stats` 一致

## 它到底读哪个文件

Docker 的 `json-file` 驱动把日志写成：

```
<data-root>/containers/<container-id>/<container-id>-json.log
```

`<data-root>` 默认是 `/var/lib/docker`，但绝大多数生产环境会在 `/etc/docker/daemon.json` 里改掉，
比如本项目作者的环境是 `/app/docker/data`。所以启动时会按下面顺序探测，**取第一个真实存在的目录**：

| 顺序 | 来源 | 说明 |
|---|---|---|
| 1 | `-data-root` 参数 | 显式指定，优先级最高；指定了但不存在会直接报错 |
| 2 | `/etc/docker/daemon.json` 的 `data-root` | 也兼容 Docker 18.03 之前的旧字段 `graph` |
| 3 | `docker info --format '{{.DockerRootDir}}'` | 权威值，但需要环境里有 docker CLI |
| 4 | `/var/lib/docker`、snap 路径 | 编译期兜底 |

探测过程会打在启动日志里，也能通过 `GET /api/meta` 看到。排查"为什么页面是空的"时先看这里。

### 日志文件格式

每行是一个 JSON 对象：

```json
{"log":"hello\n","stream":"stdout","time":"2026-11-09T01:23:45.123456789Z"}
```

两个容易踩的坑，代码里都处理了：

- **TTY 容器**（`docker run -t`）不写 `stream` 字段，统一补成 stdout
- **单条日志超过 16KB** 时 Docker 会拆成多行，前几行带 `"partial":true`。直接按行展示会把一条日志切成好几段，必须按 stream 拼回去

## 快速开始

### 方式一：单文件运行（推荐先试这个）

```bash
./build.sh                      # 生成 dist/dlog-linux-amd64 和 arm64
scp dist/dlog-linux-arm64 root@server:/usr/local/bin/dlog

# 服务器上直接跑（裸机 / 有权限的机器）
dlog -listen :8080 -web-token '换成你自己的口令'
```

浏览器打开 `http://server:8080`，输入口令后进入。7 天内免登录。

不带 `-web-token` 也能跑，但启动日志会警告：任何能连到这个端口的人都能看日志。

### 方式二：Docker Compose

```bash
docker compose up -d --build
```

Compose 里的挂载有个**必须遵守的规则**：宿主机目录要按它原本的路径挂进去。

```yaml
volumes:
  - /app/docker/data:/host/app/docker/data:ro   # 冒号两边必须一致
command: ["-host-root", "/host"]
```

因为 dlog 从 `daemon.json` 读到的路径是 `/app/docker/data`（宿主机语义），
程序内部会拼成 `<host-root>/app/docker/data` 去访问。挂到 `/logs` 之类的地方就会全部落空。

## 参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-listen` | `:8080` | HTTP 监听地址 |
| `-web-token` | 空 | 访问口令。留空则不鉴权 |
| `-data-root` | 自动探测 | 强制指定 Docker 数据根目录 |
| `-host-root` | 空 | 容器内部署时宿主机根目录的挂载点，如 `/host` |
| `-daemon-json` | `/etc/docker/daemon.json` | 自定义 daemon.json 路径 |
| `-docker-sock` | `/var/run/docker.sock` | 兜底通道与资源采集用的 socket |
| `-tail-max` | `5000` | 单次最多返回多少条历史日志 |
| `-tail-bytes` | `16777216` | 回溯历史时最多扫描的字节数 |
| `-poll-ms` | `250` | 日志文件轮询间隔（毫秒） |
| `-stats-ms` | `2000` | 资源占用采集间隔（毫秒） |
| `-no-cli` | `false` | 不调用 docker 命令探测 |
| `-no-api` | `false` | 完全禁用 socket：只读本地文件，也不显示资源占用 |
| `-no-stats` | `false` | 保留 socket 但只不采集资源占用 |

### 首屏历史行数

进入容器时默认向上回溯 **400 行**——够看清最近的请求链路，又不至于让首屏推太多行。
工具栏的「历史」下拉框可以改成 200 / 400 / 1000 / 2000 / 5000，选择会存进浏览器
`localStorage`，下次打开沿用。

- 改完立即按新行数重新拉取，不用刷新页面
- 候选项由服务端 `-tail-max` 决定，超出的请求会被夹紧，所以不会出现"选了不生效"
- 页面最多保留 `max(3000, 所选行数)` 行，实时日志持续追加，超出后从头部裁掉
- 想看比 `-tail-max` 更早的历史，启动时调大它，同时留意 `-tail-bytes`（默认只回溯 16MB）
- 服务端不传 `tail` 时的缺省值是 300，只影响直接调接口的场景，页面上看到的是下拉框的值

## 接口

| 路径 | 说明 |
|---|---|
| `POST /api/login` | 提交口令换取会话 Cookie |
| `GET /api/logout` | 清除会话 |
| `GET /api/meta` | data-root、探测来源、探测过程、socket 与采集可用性 |
| `GET /api/containers` | 容器列表（名字、镜像、状态、日志驱动、日志大小） |
| `GET /api/logs?container=x&tail=200` | 拉最后 N 条，返回 `{container, entries}` |
| `GET /api/stream?container=x&tail=300` | SSE 实时流，先补历史再跟随 |
| `GET /api/download?container=x` | 下载原始日志文件 |
| `GET /api/stats` | 所有运行中容器的 CPU/内存/网络/IO 快照 |

`container` 支持完整 ID、12 位短 ID、容器名，以及无歧义的 ID 前缀。

SSE 每条日志带 `id: <纳秒时间戳>`，浏览器断线重连会自动带 `Last-Event-ID`，
服务端据此去重，不会重复推历史。

## 访问控制

设置 `-web-token` 后，页面和所有接口都要求先验证。三种通行方式：

| 方式 | 场景 |
|---|---|
| 登录页输入口令 → 种 Cookie（HttpOnly，7 天） | 浏览器日常使用 |
| `http://host:8080/?token=xxx` | 临时分享一条链接，访问后自动种 Cookie |
| `Authorization: Bearer xxx` | 脚本 / 自动化，不会把口令留在 URL 和访问日志里 |

Cookie 里存的是口令的 SHA-256 派生值而不是口令本身，`HttpOnly` + `SameSite=Lax`；
比较用常数时间，登录失败固定延迟 300ms。**改口令等于让所有已登录会话立即失效。**

需要说明的是：`-web-token` 只解决"谁能看"，不加密传输。放到公网请套 HTTPS 反代，
否则口令和日志都是明文过网。

## 资源占用

容器名右侧显示 CPU 和内存，数据来自 `GET /containers/{id}/stats`，口径与 `docker stats` 完全一致：

- **CPU**：`(容器 CPU 增量 / 宿主机 CPU 增量) × 核数 × 100`，单核跑满 = 100%，4 核跑满 = 400%
- **内存**：`usage - inactive_file`（web 集），不是原始 usage，否则只要读过文件就会虚高

采集器每 2 秒在后台采一轮并缓存，前端每 3 秒取一次快照。CPU 必须两次采样才能算增量，
所以**服务启动后约 2~4 秒才会有第一个非零数字**，这是正常的。

不挂 docker socket 时这块直接不显示，其余功能不受影响。

## 日志跟随

默认跟随最新日志。往上滚鼠标会立刻退出跟随（顶部出现「已暂停 · N 条新日志」），
滚回最后一行会自动恢复。实现上判断的是**滚动方向**而不是"是否在底部"——
自动滚动时 `scrollTop` 只增不减，用户往上滚才会减小，两者天然可分，
不会因为布局的半像素误差误判成"用户走了"，之后再也不滚。

## 哪些情况读不到

| 情况 | 表现 | 处理 |
|---|---|---|
| 日志驱动是 `journald` / `syslog` / `fluentd` 等 | 磁盘上没有日志文件 | 需要挂 socket 走 API 兜底，或 `-no-api` 时直接提示不可读 |
| 容器目录权限不足 | 容器列表为空 | dlog 需以 root 运行（`containers/<id>/` 是 710 且属主 root） |
| `daemon.json` 里改了 data-root 但目录没挂对 | 列表为空，meta 显示探测失败 | 按上文的路径一致原则重新挂载 |
| 日志轮转（`max-size` / `max-file`） | 轮转瞬间可能丢几行 | 有意取舍，Dozzle 也一样；要完整归档请用 `docker logs` 导出 |
| 没挂 docker socket | 不显示 CPU/内存，`journald` 容器不可读 | 需要资源占用就挂 socket；只读文件可加 `-no-api` 彻底断开 |

## 目录结构

```
cmd/dlog/main.go              # 参数、启动、优雅退出
internal/dockerpath/          # data-root 探测 + hostRoot 路径映射
internal/containers/          # 扫描 containers/*/config.v2.json
internal/logsrc/
  entry.go                    # NDJSON 解析、partial 多行拼接
  tail.go                     # 从文件尾部反向读 N 行（不整文件加载）
  follow.go                   # 增量轮询、轮转/截断检测
  apisrc.go                   # docker.sock 兜底、多路复用流解帧、stats 拉取
internal/stats/               # 后台采集容器 CPU/内存，缓存快照
internal/server/
  server.go                   # HTTP API + SSE
  auth.go                     # 访问口令、会话 Cookie、鉴权中间件
internal/web/static/          # 单文件前端 + 登录页，go:embed 打进二进制
```

## 开发

```bash
go test ./...       # 单测覆盖路径探测、config 解析、尾部读取、partial 拼接、HTTP 接口
go vet ./...
./build.sh          # 交叉编译 amd64 + arm64
```
