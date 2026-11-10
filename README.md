# dlog · Docker 本地日志查看器

用 Go 写的单文件日志查看器，**直接读 Docker 写在磁盘上的容器日志文件**，浏览器里实时跟随。

和 Dozzle / Plume 这类工具的区别：它不代理 `docker logs`，而是自己解析 `json-file` 的 NDJSON。
代价是只读、不能 exec，好处是**不依赖 docker 守护进程**——挂不挂 socket 都能跑，权限面小得多。

- 零第三方依赖，纯标准库
- 二进制内嵌前端，单个静态 ELF（`CGO_ENABLED=0`），scp 到服务器就能跑
- 自动从 docker socket / `daemon.json` / `docker info` / 默认路径四级定位 data-root
- **挂载配错不用猜**：从 socket 问出宿主机真实路径，读不到就把该加的 `volumes` 行直接打印在日志和页面上
- 可选访问口令（`-web-token`），防止把日志页面裸奔在公网上
- 容器名旁边显示实时 CPU / 内存占用，口径与 `docker stats` 一致

## 它到底读哪个文件

Docker 的 `json-file` 驱动把日志写成：

```
<data-root>/containers/<container-id>/<container-id>-json.log
```

`<data-root>` 默认是 `/var/lib/docker`，但绝大多数生产环境会改掉，
比如本项目作者的环境是 `/app/docker/data`。所以启动时会按下面顺序探测，**取第一个真实存在的目录**：

| 顺序 | 来源 | 说明 |
|---|---|---|
| 1 | `-data-root` 参数 | 显式指定，优先级最高；指定了但不存在会直接报错 |
| 2 | **docker socket 的 `GET /info`** | 最权威：反映守护进程**实际生效**的配置，包括 `--data-root` 命令行参数、systemd drop-in 覆盖。挂了 socket 就不必再挂 `daemon.json` |
| 3 | `/etc/docker/daemon.json` 的 `data-root` | 配置文件里的值，也兼容 Docker 18.03 之前的旧字段 `graph` |
| 4 | `docker info --format '{{.DockerRootDir}}'` | 和顺序 2 同一个值，但需要环境里有 docker CLI（容器镜像里通常没有） |
| 5 | `/var/lib/docker`、snap 路径 | 编译期兜底 |

> 为什么 socket 排在 daemon.json 前面：配置文件写的不一定是跑起来的配置。有人用
> `dockerd --data-root=/data/docker` 或 systemd 的 `ExecStart` 覆盖，这时 daemon.json 里可能是空值
> 或者早已过时。`/info` 返回的是守护进程自己认的那个目录。

探测过程会打在启动日志里，也能通过 `GET /api/meta` 看到（`source` 字段会标明是
`flag` / `docker-api` / `daemon.json` / `docker-cli` / `default`）。排查"为什么页面是空的"时先看这里。

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

挂载只需要记住一条规则：**冒号右边必须是 `<host-root>` + 宿主机原路径**。
因为 `/info` 返回的是宿主机语义的路径（如 `/app/docker/data`），程序内部会拼成
`/host/app/docker/data` 去访问。挂到 `/logs` 这类位置会全部落空。

```yaml
volumes:
  - ${DLOG_DATA_ROOT:-/var/lib/docker}:/host${DLOG_DATA_ROOT:-/var/lib/docker}:ro
  - /var/run/docker.sock:/var/run/docker.sock:ro
command: ["-host-root", "/host"]
```

只用一个变量 `DLOG_DATA_ROOT` 表达宿主机上的 data-root，冒号右边由它自动拼出来，
所以两边不可能不一致。默认值 `/var/lib/docker` 覆盖标准安装。

## 路径不用猜：起不来会告诉你该挂哪儿

换一台机器，data-root 可能是 `/var/lib/docker`，也可能被改成 `/app/docker/data`、
`/data/docker`。dlog 不会让你先去 `cat /etc/docker/daemon.json`，而是：

1. 挂上 socket 启动，它 `GET /info` 问出守护进程**实际生效**的 data-root，
   再检查这个路径在容器里到底读不读得到；
2. **读不到不会退出**（`os.Exit(1)` 只会留下一个起不来的容器），而是把结论打成
   一段可复制的配置写进日志，页面顶部同时渲染成提示卡片（带"复制"按钮）；
3. 照着把那一行贴进 `volumes`，`docker compose up -d` 重建容器即可。

控制台里长这样：

```
================ 挂载诊断 ================
守护进程报告宿主机 data-root 是 /app/docker/data，但容器里读不到 /host/app/docker/data。
宿主机 /app/docker/data  →  容器内 /host/app/docker/data（来源 docker-api）

然后把 Docker 数据目录挂进来（冒号两边必须指向同一个真实路径）：
    volumes:
      - /app/docker/data:/host/app/docker/data:ro

等价的 docker run 写法：
    docker run ... -v /app/docker/data:/host/app/docker/data:ro ...

改完执行：docker compose up -d —— 必须重建容器，docker restart 不会应用新的挂载
==========================================
```

诊断会区分四种情况，给出的建议也不同：

| 诊断 | 含义 | 建议 |
|---|---|---|
| `not-mounted` | 从 socket 问到了宿主机路径，但容器里读不到 | 给出该路径对应的 `volumes` 行 |
| `wrong-dir` | 目录挂进来了，但里面没有 `containers/` | 提示可能挂错了位置，或这台机器还没建过容器 |
| `no-socket` | 连 socket 都没挂，问不出真实路径 | 先给出 socket 的挂载行，再附默认路径作对照 |
| `not-found` | 裸机运行也找不到 | 提示检查 Docker 是否启动、是否以 root 运行 |

**daemon.json 不用挂。** `/info` 已经包含 `DockerRootDir` 和 `LoggingDriver`，
比读配置文件更准（配置文件里写的可能被命令行参数覆盖掉）。只有禁用 socket
（`-no-api` 纯文件模式）时才需要它：

```yaml
  - /etc/docker/daemon.json:/host/etc/docker/daemon.json:ro   # 可选
```

注意宿主机没有这个文件时，这行会让 docker 建一个同名**目录**，反而导致解析失败。

## 参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-listen` | `:8080` | HTTP 监听地址 |
| `-web-token` | 空 | 访问口令。留空则不鉴权 |
| `-data-root` | 自动探测 | 强制指定 Docker 数据根目录 |
| `-host-root` | 空 | 容器内部署时宿主机根目录的挂载点，如 `/host` |
| `-daemon-json` | `/etc/docker/daemon.json` | 自定义 daemon.json 路径（没挂 socket 时才用得上） |
| `-docker-sock` | `/var/run/docker.sock` | data-root 探测、日志兜底、资源采集都用它 |
| `-tail-max` | `5000` | 单次最多返回多少条历史日志 |
| `-tail-bytes` | `16777216` | 回溯历史时最多扫描的字节数 |
| `-poll-ms` | `250` | 日志文件轮询间隔（毫秒） |
| `-stats-ms` | `2000` | 资源占用采集间隔（毫秒） |
| `-no-cli` | `false` | 不调用 docker 命令探测（socket 探测不受影响） |
| `-no-api` | `false` | 完全禁用 socket：只读本地文件，data-root 只能靠 daemon.json 或参数 |
| `-no-stats` | `false` | 保留 socket 但只不采集资源占用 |

### 首屏历史行数

进入容器时默认向上回溯 **400 行**——够看清最近的请求链路，又不至于让首屏推太多行。
工具栏的「历史」下拉框可以改成 200 / 400 / 1000 / 2000 / 5000，选择会存进浏览器
`localStorage`，下次打开沿用。

- 改完立即按新行数重新拉取，不用刷新页面
- 候选项由服务端 `-tail-max` 决定，超出的请求会被夹紧，所以不会出现"选了不生效"
- 页面最多保留 `max(3000, 所选行数)` 行，实时日志持续追加，超出后从头部裁掉
- 想看比 `-tail-max` 更早的历史，不必动这个参数——用「时间段」查那一小段更省事
  （见下文）；真要整段回溯，再调大它并留意 `-tail-bytes`（默认只回溯 16MB）
- 服务端不传 `tail` 时的缺省值是 300，只影响直接调接口的场景，页面上看到的是下拉框的值

### 时间列格式

每行左边显示日志时间，工具栏的「时间」按钮点一下换一种，选择存进 `localStorage`：

| 格式 | 显示 | 说明 |
|---|---|---|
| 日期+时间（默认） | `2026-11-10 13:48:00.003` | 本地时区，跨天的日志也能分清 |
| UTC | `2026-11-10 05:48:00.003` | 与 `docker logs` 里显示的时间**同值**，方便对照 |
| 仅时间 | `13:48:00.003` | 省横向空间，看密集日志时用 |

docker 写进日志文件的是 UTC 纳秒时间戳（`2026-11-10T05:48:00.003429131Z`），
而服务器本地通常是东八区，两者差 8 小时。默认显示本地时间更好读，需要跟
docker 原生日志核对时切到 UTC 档即可。

鼠标悬停在时间上会同时给出本地、UTC 和原始串三种值。

另外，接口返回的 `tsText` 是日志文件里的**原始时间串**，不做任何改写。
之所以不只用 `ts`（Unix 纳秒数字），是因为纳秒时间戳约 `1.7e18`，超过
JavaScript 的安全整数上限 `9e15`，经过 JSON 数字传到浏览器必然丢精度——
对比一下就知道：

```
ts     1794289683003429000   ← 后三位已被 double 吃掉
tsText 2026-11-10T05:48:03.003429182Z   ← 原样
```

显示到毫秒时两者看不出差别，但要做"这一行和 docker logs 里哪一条是同一个"
这类对齐，只有原串靠得住。

### 时间段查询

工具栏关键字框右边的「时间段」按钮，点开是一个浮层：

- **快捷档**：最近 1 / 5 / 30 分钟 / 1 小时 / 今天。点一下立即生效，只设下界，
  所以新产生的日志会继续进来，不会变成一个卡住的静态窗口
- **自定义**：填起始和结束时间（精确到秒），两端都可以留空，留空即不限
- **清除**：回到实时日志

**它不是"在已加载的几百行里再筛一遍"**，而是回日志文件里按时间戳定位：

```
选 13:10:00 ~ 13:12:00
        │
        ├─ 二分查找：找到第一条时间 ≥ 13:10:00 的行在文件里的字节偏移
        ├─ 从那里顺序往后读，读到第一条晚于 13:12:00 的行就停
        └─ 返回这个窗口内的**全部**日志（与「历史」下拉框无关）
```

Docker 的 `json-file` 是严格按时间追加写的，文件偏移和日志时间单调对应，
所以这个跳跃是二分查找而不是全文件扫描。实测 63 MB / 40 万行的文件：

| 查询 | 耗时 | 结果 |
|---|---|---|
| 文件中部 60 秒窗口 | 12 ms | 601 行 |
| 靠尾部 60 秒窗口 | 5 ms | 601 行 |
| 从文件开头起（全量） | 435 ms | 40 万行，只回传前 2 万行并标注截断 |
| 日志时间跨度（`/api/span`） | 2 ms | 首行 / 末行时间 |

所以「历史 N 行」和「时间段」是两件不同的事：

| 条件 | 作用范围 | 谁执行 |
|---|---|---|
| 历史 N 行 | 只是**实时模式**下预加载多少行 | 前端请求、服务端按尾部读 |
| 时间段 | 一个独立查询，结果条数由窗口内的日志量决定 | 服务端二分定位 + 顺序读 |
| 关键字 | 在**当前结果集**里再筛 | 前端 |

具体行为：

- 只填「从」→ 仍走实时流，历史从这个时间点开始，之后新产生的日志继续追加，
  徽标显示「实时 · 时段」
- 填了「到」→ 上界是固定时刻，之后不会再有行落进来，于是取一次快照就好，
  徽标显示「时段快照」
- 时段生效时「历史」下拉框会置灰，因为此时取多少行由时间窗口决定
- 单次最多回传 2 万条、最多向后扫 256 MiB；超了会提示"该时段共 N 行，
  只载入了前 M 行"，想全看就把时间段收窄
- 浮层标题右边显示当前容器日志文件的**实际时间范围**（如
  「日志范围 11-10 13:00:00 ~ 11-10 13:24:59 · 282KB」）和大小，
  设时间之前就知道要查的那段在不在里面
- 查出来是 0 条时，提示会带上这个范围：
  「该时间段没有日志；这个容器的日志范围是 11-10 13:00:00 ~ 11-10 13:24:59」
  而不是让你对着空白猜

关键字、级别与时段是**叠加**关系：时段最后落到 `entries` 里，关键字在结果里再筛，
计数显示成「命中 / 该时段载入」。

## 接口

| 路径 | 说明 |
|---|---|
| `POST /api/login` | 提交口令换取会话 Cookie |
| `GET /api/logout` | 清除会话 |
| `GET /api/meta` | data-root、探测来源、探测过程、socket 与采集可用性，以及 `mount` 挂载诊断（含可直接粘贴的 `volume` / `runArg`） |
| `GET /api/containers` | 容器列表（名字、镜像、状态、日志驱动、日志大小） |
| `GET /api/logs?container=x&tail=200` | 拉最后 N 条，返回 `{container, entries}` |
| `GET /api/logs?container=x&from=…&to=…` | 按时间段查日志（毫秒时间戳），返回 `{entries, total, truncated, cut, firstAt, lastAt}` |
| `GET /api/span?container=x` | 日志文件覆盖的时间范围与大小（只读首行和尾块） |
| `GET /api/stream?container=x&tail=300` | SSE 实时流，先补历史再跟随 |
| `GET /api/stream?container=x&from=…` | 同上，但历史来自时间段查询；会先发一条 `event: meta` |
| `GET /api/download?container=x` | 下载原始日志文件 |
| `GET /api/stats` | 所有运行中容器的 CPU/内存/网络/IO 快照 |

`container` 支持完整 ID、12 位短 ID、容器名，以及无歧义的 ID 前缀。

每条日志的结构：

```json
{
  "ts": 1794289683003429000,
  "tsText": "2026-11-10T05:48:03.003429182Z",
  "stream": "stderr",
  "log": "[ERROR] 连接 PACS 超时\n"
}
```

`ts` 是 Unix 纳秒（方便排序、算间隔），`tsText` 是日志文件里的原始时间串
（UTC，保留纳秒，用于精确显示与对照）；`stream` 取 `stdout` / `stderr` / `raw`，
`raw` 表示这一行不是合法 JSON，按纯文本兜底显示。

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
| data-root 换了位置但没按原路径挂 | 页面顶部出现「日志目录没有挂进来」，并给出该加的 `volumes` 行 | 照抄那一行 → `docker compose up -d` 重建容器 |
| 挂到了 `/logs` 之类的位置 | 目录在但空，诊断为 `wrong-dir` | 改成 `<data-root>:<host-root><data-root>:ro` |
| 完全没挂 socket | 诊断为 `no-socket`，容器列表为空 | 先挂 socket（dlog 才知道真实路径），只读文件可 `-no-api` |
| 日志轮转（`max-size` / `max-file`） | 轮转瞬间可能丢几行 | 有意取舍，Dozzle 也一样；要完整归档请用 `docker logs` 导出 |

## 目录结构

```
cmd/dlog/main.go              # 参数、启动、优雅退出
internal/dockerpath/          # data-root 探测 + hostRoot 路径映射
internal/containers/          # 扫描 containers/*/config.v2.json
internal/logsrc/
  entry.go                    # NDJSON 解析、partial 多行拼接
  tail.go                     # 从文件尾部反向读 N 行（不整文件加载）
  range.go                    # 按时间段取日志：二分定位起始偏移 + 顺序读
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
