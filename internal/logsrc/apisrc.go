package logsrc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DockerAPI 是日志兜底通道：当日志驱动不是 json-file/local（例如 journald、
// syslog、fluentd）时，磁盘上根本没有文件可读，只能问 Docker 守护进程要。
//
// 直接用 HTTP over unix socket 拼请求，不引入 docker/docker 客户端库——
// 那会带进几十个依赖，而我们只需要一个端点。
type DockerAPI struct {
	socket string
	client *http.Client
}

// NewDockerAPI 创建客户端，socket 一般是 /var/run/docker.sock。
//
// 也接受 tcp://host:port 形式：既方便单元测试用 httptest 顶替守护进程，
// 也能在确有必要时连到别处的 daemon（生产环境请坚持用 unix socket）。
//
// 如果没挂 socket，这里不会报错，要等第一次请求时才会暴露（让 -no-api 的
// 纯文件模式可以完全不受影响）。
func NewDockerAPI(socket string) *DockerAPI {
	a := &DockerAPI{socket: socket}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return a.dial(ctx)
		},
		// 跟随日志是长连接，禁掉 keep-alive 免得连接池里堆一堆死连接。
		DisableKeepAlives: true,
	}
	a.client = &http.Client{Transport: tr}
	return a
}

// dial 按 socket 的形式选择 unix 还是 tcp。
func (a *DockerAPI) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	if rest, ok := strings.CutPrefix(a.socket, "tcp://"); ok {
		return d.DialContext(ctx, "tcp", rest)
	}
	return d.DialContext(ctx, "unix", a.socket)
}

// Socket 返回配置的 socket 路径。
func (a *DockerAPI) Socket() string { return a.socket }

// Available 探测 socket 是否可连接，用于启动时决定要不要启用兜底通道。
func (a *DockerAPI) Available(timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, err := a.dial(ctx)
	if err != nil {
		return fmt.Errorf("连接 %s 失败: %w", a.socket, err)
	}
	return conn.Close()
}

// Logs 拉取容器日志。
//
// 返回的 Reader 已经把 Docker 的多路复用帧拆掉了（TTY 容器本来就是裸流，
// 不需要拆），调用方直接按行读即可。开启 timestamps 后每行形如
// "2026-11-09T01:23:45.123456789Z 正文"，用 ParseAPILine 解析。
func (a *DockerAPI) Logs(ctx context.Context, id string, tail int, follow, tty bool) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("timestamps", "1")
	q.Set("tail", strconv.Itoa(tail))
	if follow {
		q.Set("follow", "1")
	}

	// 主机名随便写，Transport 会无视它直接连 socket。
	target := "http://docker/containers/" + url.PathEscape(id) + "/logs?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("访问 docker socket（%s）失败: %w", a.socket, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("docker API 返回 %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	if tty {
		// TTY 容器没有 8 字节帧头，直接读裸流。
		return resp.Body, nil
	}
	return struct {
		io.Reader
		io.Closer
	}{
		Reader: &muxReader{src: bufio.NewReaderSize(resp.Body, 64<<10)},
		Closer: resp.Body,
	}, nil
}

// muxReader 拆解 Docker 的多路复用流。
//
// 每个帧的格式是 8 字节头 + 负载：
//
//	[0]   流类型：0=stdin 1=stdout 2=stderr
//	[1:4] 保留
//	[4:8] 负载长度，big-endian
//
// 这里把 stdout 和 stderr 合并成一路输出——日志本来就该按时间混排，
// 只有 stream 分别展示才需要区分，而走 API 兜底时通常只想看内容。
type muxReader struct {
	src *bufio.Reader
	buf bytes.Buffer
	err error
}

func (m *muxReader) Read(p []byte) (int, error) {
	for m.buf.Len() == 0 {
		if m.err != nil {
			return 0, m.err
		}
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(m.src, hdr); err != nil {
			m.err = err
			return 0, err
		}
		size := binary.BigEndian.Uint32(hdr[4:])
		if size == 0 {
			continue
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(m.src, payload); err != nil {
			m.err = err
			return 0, err
		}
		m.buf.Write(payload)
	}
	return m.buf.Read(p)
}

// ParseAPILine 解析带 timestamps 的一行，例如
// "2026-11-09T01:23:45.123456789Z hello world"。
func ParseAPILine(line string) Entry {
	line = strings.TrimRight(line, "\r")
	if line == "" {
		return Entry{}
	}

	ts, rest := splitTimestamp(line)
	return Entry{Ts: ts, Stream: "stdout", Log: rest}
}

// splitTimestamp 把行首的 RFC3339Nano 时间戳和正文分开。
// 时间戳里不会有空格，所以第一个空格就是分隔点。
func splitTimestamp(line string) (int64, string) {
	sp := strings.IndexByte(line, ' ')
	if sp <= 0 {
		return 0, line
	}
	t, err := time.Parse(time.RFC3339Nano, line[:sp])
	if err != nil {
		// 不是时间戳就整行当正文，别把内容吃掉。
		return 0, line
	}
	return t.UnixNano(), line[sp+1:]
}

// ContainerStats 是 /containers/{id}/stats 响应里我们用得到的字段。
// 其余（throttling、hugetlb、storage 等）忽略。
type ContainerStats struct {
	Read     time.Time `json:"read"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PercpuUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs  uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IoServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	PidsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

// Stats 拉取容器的一次资源快照。
//
// stream=false 让守护进程返回单个 JSON 而不是长连接流；one-shot=true 让它跳过
// "采样 1 秒再返回"的等待（API 1.41+ 才认这个参数，更老的版本会退化成等 1 秒，
// 仍然能拿到数据，只是慢一点）。
//
// 这里刻意不用响应里的 precpu_stats 算 CPU：one-shot 模式下它可能是空值，
// 由调用方自己保存上一次采样做差更可靠。
func (a *DockerAPI) Stats(ctx context.Context, id string) (*ContainerStats, error) {
	target := "http://docker/containers/" + url.PathEscape(id) + "/stats?stream=false&one-shot=true"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("访问 docker socket（%s）失败: %w", a.socket, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("docker stats 返回 %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var out ContainerStats
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("解析 stats 响应失败: %w", err)
	}
	return &out, nil
}
