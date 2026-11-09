// Package dockerpath 负责找到 Docker 的数据根目录（data-root），并把它
// 换算成"当前进程真的能读到"的文件系统路径。
//
// 之所以要单独一个包，是因为 dlog 的核心前提是「直接读本地日志文件」，
// 而日志文件在哪完全取决于 Docker 守护进程怎么配的：
//
//   - 默认是 /var/lib/docker
//   - 绝大多数生产环境会在 /etc/docker/daemon.json 里改成别的盘（比如 /app/docker/data）
//   - snap 装的是 /var/snap/docker/common/var-lib-docker
//
// 另外 dlog 自己通常也是跑在容器里的，那时候宿主机的 /app/docker/data
// 会被挂到 /host/app/docker/data，所以还需要一层 hostRoot 前缀映射。
package dockerpath

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

// Source 标明 data-root 最终是从哪一级推断出来的，用于启动日志和 /api/meta 展示。
type Source string

const (
	SourceFlag       Source = "flag"        // 启动参数 -data-root 显式指定
	SourceDockerAPI  Source = "docker-api"  // docker socket 的 GET /info
	SourceDaemonJSON Source = "daemon.json" // /etc/docker/daemon.json
	SourceDockerCLI  Source = "docker-cli"  // docker info --format
	SourceDefault    Source = "default"     // 编译期兜底的默认路径
)

// defaultRoots 是标准安装位置，按优先级排列。
// snap 版本放在后面：只有常规路径不存在时才轮到它。
var defaultRoots = []string{
	"/var/lib/docker",
	"/var/snap/docker/common/var-lib-docker",
}

// DaemonConfig 只保留我们关心的字段，其余忽略。
// 注意 data-root 与 graph 并存：graph 是 Docker 18.03 之前的旧字段名，
// 老机器上仍可能遇到，所以两个都认。
type DaemonConfig struct {
	DataRoot  string            `json:"data-root"`
	Graph     string            `json:"graph"`
	LogDriver string            `json:"log-driver"`
	LogOpts   map[string]string `json:"log-opts"`
}

// Root 返回配置里声明的数据根目录，data-root 优先于老的 graph 字段。
func (d DaemonConfig) Root() (string, Source) {
	if s := strings.TrimSpace(d.DataRoot); s != "" {
		return s, SourceDaemonJSON
	}
	if s := strings.TrimSpace(d.Graph); s != "" {
		return s, SourceDaemonJSON
	}
	return "", ""
}

// Driver 返回守护进程默认的日志驱动，未配置时 Docker 实际就是 json-file。
func (d DaemonConfig) Driver() string {
	if s := strings.TrimSpace(d.LogDriver); s != "" {
		return s
	}
	return "json-file"
}

// SocketInfo 是通过 docker socket 问到的守护进程配置。
// 定义在这里而不是直接引用 logsrc，是为了避免两个包互相依赖。
type SocketInfo struct {
	// DataRoot 是宿主机语义的 data-root（如 /app/docker/data）。
	DataRoot string
	// LogDriver 是默认日志驱动。
	LogDriver string
}

// MountKind 是挂载诊断的结论分类，前端按它决定标题文案。
type MountKind string

const (
	MountOK       MountKind = "ok"          // 目录可读且结构正常
	MountNotMount MountKind = "not-mounted" // 拿到了宿主机路径，但容器里读不到 → 挂载漏了或路径不一致
	MountWrongDir MountKind = "wrong-dir"   // 路径存在但不像 data-root（缺 containers/ 子目录）
	MountNoSocket MountKind = "no-socket"   // 没有 socket 也没有配置文件，无从得知真实 data-root
	MountNotFound MountKind = "not-found"   // 裸机运行且确实找不到
)

// MountCheck 是"宿主机 data-root 在当前进程里到底挂没挂上"的诊断结果。
//
// 容器化部署时最常见的失败不是权限，而是路径挂错了位置：程序从 socket 问得到
// 宿主机的 /app/docker/data，但容器里那个位置是空的。这时候报一句 not found
// 毫无帮助，真正有用的是"在 volumes 里加这一行"。所以诊断结果里直接带上
// 可粘贴的配置片段。
type MountCheck struct {
	OK bool `json:"ok"`
	// Kind 见上面的常量。
	Kind MountKind `json:"kind"`
	// HostPath 是宿主机语义的 data-root，也就是 -v 的左边。
	HostPath string `json:"hostPath,omitempty"`
	// ContainerPath 是当前进程试图访问的路径，也就是 -v 的右边。
	ContainerPath string `json:"containerPath,omitempty"`
	// Source 是 HostPath 的来源（docker-api / daemon.json / default ...）。
	Source Source `json:"source,omitempty"`
	// Message 是一句话说明现状。
	Message string `json:"message"`
	// Volume 可以直接粘进 compose 的 volumes 行（不含前面的 "- "）。
	Volume string `json:"volume,omitempty"`
	// RunArg 是等价的 docker run -v 参数。
	RunArg string `json:"runArg,omitempty"`
	// SocketVolume 是 docker socket 的挂载行，仅在连 socket 都没挂时给出。
	SocketVolume string `json:"socketVolume,omitempty"`
	// Restart 是改完配置后该执行的命令。
	Restart string `json:"restart,omitempty"`
}

// Options 是 Resolve 的输入。
type Options struct {
	// DataRoot 是用户显式指定的宿主机路径，一旦指定就必须存在，不再兜底。
	DataRoot string
	// HostRoot 是容器内部署时宿主机根目录的挂载点（如 /host），裸机运行为空。
	HostRoot string
	// DaemonJSON 自定义 daemon.json 路径，留空则用 <hostRoot>/etc/docker/daemon.json。
	DaemonJSON string
	// ProbeInfo 通过 docker socket 读取守护进程实际配置（GET /info）。
	// 为 nil 表示没有 socket 可用（-no-api 或没挂），此时只能靠文件和 CLI。
	// 拿到的是最权威的值：它反映守护进程真正在用的配置，而不是配置文件写了什么。
	ProbeInfo func(ctx context.Context) (SocketInfo, error)
	// SkipCLI 为 true 时不再尝试调用 docker 命令（纯文件模式/无 docker CLI 的环境）。
	SkipCLI bool
	// Timeout 是 socket 探测与调用 docker CLI 的超时，默认 3 秒。
	Timeout time.Duration
}

// Resolver 是探测结果。
type Resolver struct {
	// DataRoot 是宿主机语义的路径，例如 /app/docker/data。
	DataRoot string
	// HostRoot 是宿主机根目录在当前进程里的挂载点，裸机为空串。
	HostRoot string
	// Source 标明 DataRoot 的来源。
	Source Source
	// Daemon 是解析到的 daemon.json 内容（可能为空值）。
	Daemon DaemonConfig
	// Mount 是"宿主机路径到底挂没挂上"的诊断，无论解析成功与否都会被填。
	// 失败场景下它就是修复指引，前端会直接渲染成可复制的配置片段。
	Mount MountCheck
	// Notes 按顺序记录每一级探测的成败，排查"为什么日志是空的"时非常有用。
	Notes []string
}

// Resolve 按 flag → docker socket → daemon.json → docker info → 默认路径
// 的顺序定位 data-root。
//
// 只有「目录真实存在」的候选才会被采纳，否则继续往下试；全部失败则返回错误，
// 错误信息里带上每一步的原因，避免用户只看到一句没有上下文的 not found。
func Resolve(o Options) (*Resolver, error) {
	r := &Resolver{HostRoot: normalizeHostRoot(o.HostRoot)}

	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	// ---- 第一级：读 daemon.json，顺便拿到默认日志驱动 ----
	djPath := o.DaemonJSON
	if djPath == "" {
		djPath = r.hostPath("/etc/docker/daemon.json")
	}
	if data, err := os.ReadFile(djPath); err != nil {
		r.Notes = append(r.Notes, fmt.Sprintf("读取 %s 失败：%v", djPath, err))
	} else if err := json.Unmarshal(data, &r.Daemon); err != nil {
		// daemon.json 内容非法不致命，Docker 自己也可能根本没在用它。
		r.Notes = append(r.Notes, fmt.Sprintf("解析 %s 失败：%v", djPath, err))
	} else {
		r.Notes = append(r.Notes, fmt.Sprintf("读取 %s 成功", djPath))
	}

	// ---- 第二级：问守护进程 ----
	// socket 可用时这一步最权威：/info 返回的是真正在跑的配置。配置文件里
	// 写的东西可能被 dockerd 命令行参数或 systemd drop-in 覆盖掉。
	var sock SocketInfo
	if o.ProbeInfo != nil {
		pctx, cancel := context.WithTimeout(context.Background(), timeout)
		info, err := o.ProbeInfo(pctx)
		cancel()
		if err != nil {
			r.Notes = append(r.Notes, "docker socket 探测跳过："+err.Error())
		} else {
			sock = info
			if s := strings.TrimSpace(info.LogDriver); s != "" {
				// 运行时的驱动比 daemon.json 里写的可信
				r.Daemon.LogDriver = s
			}
			r.Notes = append(r.Notes, fmt.Sprintf("docker socket 报告 data-root=%s 驱动=%s", info.DataRoot, info.LogDriver))
		}
	}

	// ---- 汇总候选 ----
	type candidate struct {
		root string
		src  Source
	}
	var cands []candidate

	explicit := strings.TrimSpace(o.DataRoot)
	if explicit != "" {
		cands = append(cands, candidate{normalizeDataRoot(explicit), SourceFlag})
	}
	// socket 的值排在 daemon.json 之前：两者冲突时以运行中的守护进程为准。
	if s := strings.TrimSpace(sock.DataRoot); s != "" {
		cands = append(cands, candidate{normalizeDataRoot(s), SourceDockerAPI})
	}
	if root, src := r.Daemon.Root(); root != "" {
		cands = append(cands, candidate{normalizeDataRoot(root), src})
	}
	if !o.SkipCLI {
		if root, err := probeDockerCLI(timeout); err != nil {
			r.Notes = append(r.Notes, "docker info 探测跳过："+err.Error())
		} else {
			cands = append(cands, candidate{normalizeDataRoot(root), SourceDockerCLI})
		}
	}
	for _, root := range defaultRoots {
		cands = append(cands, candidate{root, SourceDefault})
	}

	// ---- 逐个校验 ----
	// 分两轮：先找结构正常的候选（含 containers/ 子目录），都没命中再退让给
	// "目录存在但不像 data-root"的。因为"目录存在"不等于"挂对了"：把宿主机
	// 目录挂到 /logs 时 /host/app/docker/data 可能被 docker 自动建成一个空目录，
	// 这时能读到目录但读不到容器，诊断需要区分这两种情况。
	var fallback *candidate
	for _, c := range cands {
		fsPath := r.FS(c.root)
		if !isDir(fsPath) {
			r.Notes = append(r.Notes, fmt.Sprintf("候选 %s（来源 %s）不存在：%s", c.root, c.src, fsPath))
			continue
		}
		if isDir(path.Join(fsPath, "containers")) {
			r.DataRoot = c.root
			r.Source = c.src
			r.Mount = r.okMount(c.root, c.src)
			r.Notes = append(r.Notes, fmt.Sprintf("采用 %s（来源 %s）", c.root, c.src))
			return r, nil
		}
		if fallback == nil {
			cc := c
			fallback = &cc
		}
		r.Notes = append(r.Notes, fmt.Sprintf("候选 %s 存在但没有 containers/ 子目录：%s", c.root, fsPath))
	}

	// 目录在、结构不对：仍然采用它（否则页面连容器列表都渲染不出来），
	// 但标记成 wrong-dir 让前端提示"可能挂到了错误的位置"。
	if fallback != nil {
		r.DataRoot = fallback.root
		r.Source = fallback.src
		r.Mount = r.hintMount(fallback.root, fallback.src, MountWrongDir,
			fmt.Sprintf("%s 存在，但里面没有 containers/ 子目录。可能是挂载点写错了，也可能是这台机器还没创建过容器。", r.FS(fallback.root)))
		return r, nil
	}

	// 显式指定却不存在，是最常见的配置错误，单独强调一下。
	if explicit != "" {
		root := normalizeDataRoot(explicit)
		r.Mount = r.hintMount(root, SourceFlag, MountNotMount,
			fmt.Sprintf("你指定的 data-root %s 在当前环境里读不到（实际查找 %s）。", root, r.FS(root)))
		return r, fmt.Errorf("指定的 data-root %s 不可访问（实际查找路径 %s）", explicit, r.FS(explicit))
	}

	// ---- 全部落空：尽可能猜出宿主机的真实路径，给出修复指引 ----
	// 优先级和探测链一致：socket 的值最权威，其次是配置文件，最后才是默认路径。
	guess, guessSrc := "", Source("")
	if s := strings.TrimSpace(sock.DataRoot); s != "" {
		guess, guessSrc = normalizeDataRoot(s), SourceDockerAPI
	} else if root, src := r.Daemon.Root(); root != "" {
		guess, guessSrc = normalizeDataRoot(root), src
	} else if len(defaultRoots) > 0 {
		// 完全没有依据时的兜底猜测：绝大多数标准安装都在 /var/lib/docker。
		guess, guessSrc = defaultRoots[0], SourceDefault
	}

	switch {
	case r.HostRoot == "":
		// 裸机运行不需要挂载，找不到只可能是 docker 没启动或权限不够。
		r.Mount = MountCheck{
			Kind:     MountNotFound,
			HostPath: guess,
			Source:   guessSrc,
			Message:  fmt.Sprintf("在宿主机上找不到 %s。裸机部署不需要挂载，请确认 Docker 已启动，且 dlog 以 root 运行（containers/ 目录是 710 root:root，普通用户进不去）。", guess),
		}
	case guessSrc == SourceDefault:
		// 连 socket 都没有，猜出来的路径不能当作事实，先让用户把 socket 挂上。
		r.Mount = r.hintMount(guess, guessSrc, MountNoSocket,
			"docker socket 不可用，既问不到真实 data-root，也没有配置文件可读。先挂上 socket，dlog 就能直接问守护进程要路径（下面同时给出默认路径的挂法，供你对照）。")
		r.Mount.SocketVolume = "/var/run/docker.sock:/var/run/docker.sock:ro"
	default:
		r.Mount = r.hintMount(guess, guessSrc, MountNotMount,
			fmt.Sprintf("守护进程报告宿主机 data-root 是 %s，但容器里读不到 %s。说明这行挂载没加，或者冒号右边的路径和左边不一致。", guess, r.FS(guess)))
	}

	// 错误串保持简短：每一级的失败原因已经按行记在 Notes 里，
	// 再拼一遍只会让启动日志变成一堵墙。
	return r, fmt.Errorf("未能定位 Docker data-root（已尝试 %d 个候选，逐级原因见 Notes）", len(cands))
}

// okMount 生成"一切正常"的诊断。
func (r *Resolver) okMount(hostPath string, src Source) MountCheck {
	return MountCheck{
		OK:            true,
		Kind:          MountOK,
		HostPath:      hostPath,
		ContainerPath: r.FS(hostPath),
		Source:        src,
		Message:       fmt.Sprintf("已通过 %s 读取 %s", r.FS(hostPath), src),
	}
}

// hintMount 生成"读不到，教你怎么挂"的诊断。
//
// 只有容器化部署（HostRoot 非空）才需要挂载，裸机场景下 Volume/RunArg 留空，
// 免得给出一句在宿主机上毫无意义的 -v。
func (r *Resolver) hintMount(hostPath string, src Source, kind MountKind, msg string) MountCheck {
	m := MountCheck{Kind: kind, HostPath: hostPath, Source: src, Message: msg}
	if hostPath == "" {
		return m
	}
	m.ContainerPath = r.FS(hostPath)
	if r.HostRoot != "" {
		// 冒号两边必须指向同一个真实路径，否则 dlog 拼出来的路径全部落空。
		m.Volume = fmt.Sprintf("%s:%s:ro", hostPath, m.ContainerPath)
		m.RunArg = "-v " + m.Volume
		// 改 volumes 之后必须重建容器：docker restart 沿用旧配置，不会应用新挂载。
		m.Restart = "docker compose up -d —— 必须重建容器，docker restart 不会应用新的挂载"
	}
	return m
}

// FS 把宿主机语义路径换算成当前进程可访问的路径。
//
// 裸机（HostRoot 为空）原样返回；容器内则补上 /host 前缀。已经是挂载点前缀的
// 路径直接返回，避免拼出 /host/host/... 这种不存在的路径。
func (r *Resolver) FS(p string) string {
	if p == "" {
		return p
	}
	if r.HostRoot == "" {
		return p
	}
	if p == r.HostRoot || strings.HasPrefix(p, r.HostRoot+"/") {
		return p
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return r.HostRoot + p
}

// ContainersDir 返回容器元数据目录（每个容器一个子目录）。
// 定位失败时 DataRoot 为空，此时返回空串——调用方据此跳过扫描，
// 而不是拿着相对路径 "containers" 去读当前工作目录。
func (r *Resolver) ContainersDir() string {
	if r.DataRoot == "" {
		return ""
	}
	return r.FS(path.Join(r.DataRoot, "containers"))
}

// daemonJSONPath 返回 daemon.json 的可读路径，供 meta 接口展示。
func (r *Resolver) DaemonJSONPath() string {
	return r.hostPath("/etc/docker/daemon.json")
}

// hostPath 把绝对路径映射到宿主机根挂载点之下。
func (r *Resolver) hostPath(p string) string {
	if r.HostRoot == "" {
		return p
	}
	if p == "/" {
		return r.HostRoot
	}
	return r.HostRoot + p
}

// probeDockerCLI 用 docker info 拿权威值。这一步在容器里通常不可用
// （没装 CLI 或没挂 socket），所以失败是常态，调用方只把它当补充证据。
func probeDockerCLI(timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.DockerRootDir}}").Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("docker info 返回空值")
	}
	return s, nil
}

// normalizeDataRoot 去掉尾部分隔符，保证后续 path.Join 不会拼出双斜杠。
func normalizeDataRoot(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return p
}

// normalizeHostRoot 把 "/" 视为"没有前缀"，避免拼出 //app/docker/data。
func normalizeHostRoot(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimRight(p, "/")
	return p
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
