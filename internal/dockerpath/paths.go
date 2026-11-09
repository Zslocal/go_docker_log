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

// Options 是 Resolve 的输入。
type Options struct {
	// DataRoot 是用户显式指定的宿主机路径，一旦指定就必须存在，不再兜底。
	DataRoot string
	// HostRoot 是容器内部署时宿主机根目录的挂载点（如 /host），裸机运行为空。
	HostRoot string
	// DaemonJSON 自定义 daemon.json 路径，留空则用 <hostRoot>/etc/docker/daemon.json。
	DaemonJSON string
	// SkipCLI 为 true 时不再尝试调用 docker 命令（纯文件模式/无 docker CLI 的环境）。
	SkipCLI bool
	// Timeout 是调用 docker CLI 的超时，默认 3 秒。
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
	// Notes 按顺序记录每一级探测的成败，排查"为什么日志是空的"时非常有用。
	Notes []string
}

// Resolve 按 flag → daemon.json → docker info → 默认路径 的顺序定位 data-root。
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

	// ---- 逐个校验，取第一个真实存在的目录 ----
	for _, c := range cands {
		fsPath := r.FS(c.root)
		if isDir(fsPath) {
			r.DataRoot = c.root
			r.Source = c.src
			r.Notes = append(r.Notes, fmt.Sprintf("采用 %s（来源 %s）", c.root, c.src))
			return r, nil
		}
		r.Notes = append(r.Notes, fmt.Sprintf("候选 %s（来源 %s）不存在：%s", c.root, c.src, fsPath))
	}

	// 显式指定却不存在，是最常见的配置错误，单独强调一下。
	if explicit != "" {
		return r, fmt.Errorf("指定的 data-root %s 不可访问（实际查找路径 %s）", explicit, r.FS(explicit))
	}
	return r, fmt.Errorf("未能定位 Docker data-root，已尝试 %d 个候选：%s", len(cands), strings.Join(r.Notes, "；"))
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
func (r *Resolver) ContainersDir() string {
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
