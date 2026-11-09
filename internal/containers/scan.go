// Package containers 扫描 Docker 的 containers 目录，还原出容器清单。
//
// 这里刻意不走 docker.sock：只读本地文件就能拿到容器名、镜像、状态和日志驱动，
// 部署时不用挂 socket，也就少了一份把整个 Docker 控制面暴露出去的风险。
package containers

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// Container 是给前端用的容器概要，字段刻意扁平。
type Container struct {
	ID       string `json:"id"`
	ShortID  string `json:"shortId"`
	Name     string `json:"name"`
	Image    string `json:"image"`
	State    string `json:"state"`
	Running  bool   `json:"running"`
	Started  string `json:"started,omitempty"`
	Finished string `json:"finished,omitempty"`
	Compose  string `json:"compose,omitempty"`

	// LogDriver 是容器实际生效的日志驱动。
	LogDriver string `json:"logDriver"`
	// HasLogFile 为 false 表示日志不在本地文件里（journald / syslog / fluentd 等），
	// 前端会据此提示"需要 docker API 兜底"。
	HasLogFile bool  `json:"hasLogFile"`
	LogSize    int64 `json:"logSize"`
	TTY        bool  `json:"tty"`

	// LogFile 是日志文件的真实可读路径。不下发到前端（json:"-"），
	// 避免把宿主机目录结构暴露给浏览器。
	LogFile string `json:"-"`
}

// configV2 是 <data-root>/containers/<id>/config.v2.json 的子集。
// 只声明需要的字段，其余字段被 json 解码器自动忽略。
type configV2 struct {
	ID    string `json:"ID"`
	Name  string `json:"Name"`
	Image string `json:"Image"`

	Config struct {
		Image  string            `json:"Image"`
		Tty    bool              `json:"Tty"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`

	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`

	HostConfig struct {
		LogConfig struct {
			Type   string            `json:"Type"`
			Config map[string]string `json:"Config"`
		} `json:"LogConfig"`
	} `json:"HostConfig"`
}

// Scanner 负责把目录内容翻译成容器列表。
type Scanner struct {
	// Dir 是 containers 目录的可读路径（已做过 hostRoot 映射）。
	Dir string
	// DefaultDriver 是 daemon.json 里配置的默认日志驱动，
	// 容器自身上没写 LogConfig 时用它兜底。
	DefaultDriver string
}

// NewScanner 构造扫描器，defaultDriver 传空则按 json-file 处理。
func NewScanner(dir, defaultDriver string) *Scanner {
	if strings.TrimSpace(defaultDriver) == "" {
		defaultDriver = "json-file"
	}
	return &Scanner{Dir: dir, DefaultDriver: defaultDriver}
}

// List 读取目录下所有容器。
//
// 单个容器解析失败不会中断整体流程：容器正在重建、目录权限不足、
// config.v2.json 半写状态都可能发生，让整个页面 500 是最差的选择。
func (s *Scanner) List() ([]Container, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("读取容器目录 %s 失败: %w", s.Dir, err)
	}

	list := make([]Container, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := path.Join(s.Dir, e.Name())
		raw, err := os.ReadFile(path.Join(dir, "config.v2.json"))
		if err != nil {
			continue // 目录存在但元数据还没写完，跳过
		}
		var cfg configV2
		if err := json.Unmarshal(raw, &cfg); err != nil {
			continue
		}

		c := s.build(e.Name(), dir, cfg)
		list = append(list, c)
	}

	// 运行中的排前面，其次是名字；名字相同的按 ID 保证稳定顺序。
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Running != list[j].Running {
			return list[i].Running
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	return list, nil
}

// build 把一份 config.v2.json 组装成 Container。
func (s *Scanner) build(dirName, dir string, cfg configV2) Container {
	id := dirName // 目录名就是完整的 64 位 ID，比 JSON 里的字段更可靠
	if cfg.ID != "" {
		id = cfg.ID
	}

	name := strings.TrimPrefix(cfg.Name, "/")
	if name == "" {
		name = shortID(id)
	}

	image := cfg.Config.Image
	if image == "" {
		image = cfg.Image
	}

	driver := strings.TrimSpace(cfg.HostConfig.LogConfig.Type)
	if driver == "" {
		driver = s.DefaultDriver
	}

	state := cfg.State.Status
	if state == "" {
		if cfg.State.Running {
			state = "running"
		} else {
			state = "stopped"
		}
	}

	c := Container{
		ID:        id,
		ShortID:   shortID(id),
		Name:      name,
		Image:     image,
		State:     state,
		Running:   cfg.State.Running || state == "running",
		Started:   normalizeTime(cfg.State.StartedAt),
		Finished:  normalizeTime(cfg.State.FinishedAt),
		Compose:   composeProject(cfg.Config.Labels),
		LogDriver: driver,
		TTY:       cfg.Config.Tty,
	}

	if logFile := LogFile(dir, id, driver); logFile != "" {
		c.HasLogFile = true
		c.LogFile = logFile
		if st, err := os.Stat(logFile); err == nil {
			c.LogSize = st.Size()
		}
	}
	return c
}

// LogFile 返回容器日志文件的路径；该驱动不落本地文件时返回空串。
//
//   - json-file：<containers>/<id>/<id>-json.log，每行一个 JSON 对象
//   - local：<containers>/<id>/local-logs/container.log，格式与 json-file 一致，
//     差别只在轮转由 Docker 自己管、压缩存储
//
// 其余驱动（journald、syslog、fluentd、gelf、awslogs…）日志根本不在磁盘上，
// 只能走 docker API。
func LogFile(dir, id, driver string) string {
	switch driver {
	case "json-file", "":
		return path.Join(dir, id+"-json.log")
	case "local":
		return path.Join(dir, "local-logs", "container.log")
	default:
		return ""
	}
}

// composeProject 从 compose 注入的标签里取出项目名，方便按项目归类。
func composeProject(labels map[string]string) string {
	if labels == nil {
		return ""
	}
	return labels["com.docker.compose.project"]
}

// normalizeTime 把 Docker 的零值时间（0001-01-01T00:00:00Z）转成空串，
// 否则前端会显示一个公元 1 年的时间戳。
func normalizeTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "0001-01-01") {
		return ""
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Local().Format(time.RFC3339)
	}
	return s
}

// shortID 取前 12 位，与 docker ps 的显示习惯一致。
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
