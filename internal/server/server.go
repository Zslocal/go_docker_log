// Package server 把读日志的能力暴露成 HTTP 接口 + SSE 实时流。
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Zslocal/go_docker_log/internal/containers"
	"github.com/Zslocal/go_docker_log/internal/dockerpath"
	"github.com/Zslocal/go_docker_log/internal/logsrc"
	"github.com/Zslocal/go_docker_log/internal/stats"
)

// Config 是服务的可调参数，零值会被填成合理默认。
type Config struct {
	Resolver *dockerpath.Resolver
	// API 为 nil 表示禁用 docker socket 兜底，此时非 json-file/local 的容器不可读。
	API *logsrc.DockerAPI
	// Stats 为 nil 表示不采集资源占用（没挂 socket 时就是这种情况）。
	Stats *stats.Collector
	// Token 为空表示不鉴权；非空时所有页面和接口都要求先登录。
	Token string

	TailDefault  int
	TailMax      int
	TailBytes    int64
	PollInterval time.Duration
	Logger       *log.Logger
}

// Server 持有扫描器和容器列表缓存。
type Server struct {
	cfg  Config
	scan *containers.Scanner

	mu      sync.Mutex
	cache   []containers.Container
	cacheAt time.Time
}

// cacheTTL 是容器列表的缓存时间。Docker 新建/销毁容器时目录会变，
// 缓存只是为了别让前端每次过滤都去 stat 上百个文件。
const cacheTTL = 2 * time.Second

// New 构造服务。
func New(cfg Config) *Server {
	if cfg.TailDefault <= 0 {
		cfg.TailDefault = 300
	}
	if cfg.TailMax <= 0 {
		cfg.TailMax = 5000
	}
	if cfg.TailBytes <= 0 {
		cfg.TailBytes = logsrc.DefaultTailBytes
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}

	s := &Server{cfg: cfg}
	s.scan = containers.NewScanner(cfg.Resolver.ContainersDir(), cfg.Resolver.Daemon.Driver())
	return s
}

// Routes 注册所有路由。static 通常是内嵌的前端，可以为 nil。
func (s *Server) Routes(static http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/meta", s.handleMeta)
	mux.HandleFunc("/api/containers", s.handleContainers)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/stream", s.handleStream)
	mux.HandleFunc("/api/span", s.handleSpan)
	mux.HandleFunc("/api/download", s.handleDownload)
	mux.HandleFunc("/api/stats", s.handleStats)
	if static != nil {
		mux.Handle("/", static)
	}
	return s.recover(s.auth(mux))
}

// recover 兜住单个请求里的 panic，避免一个畸形日志文件把进程带走。
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.cfg.Logger.Printf("panic %s %s: %v", r.Method, r.URL.Path, v)
				writeError(w, http.StatusInternalServerError, fmt.Errorf("内部错误"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- 元信息

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	apiOK := false
	if s.cfg.API != nil {
		apiOK = s.cfg.API.Available(time.Second) == nil
	}
	resp := map[string]any{
		"dataRoot":       s.cfg.Resolver.DataRoot,
		"hostRoot":       s.cfg.Resolver.HostRoot,
		"source":         string(s.cfg.Resolver.Source),
		"defaultDriver":  s.cfg.Resolver.Daemon.Driver(),
		"containersDir":  s.cfg.Resolver.ContainersDir(),
		"daemonJSON":     s.cfg.Resolver.DaemonJSONPath(),
		"notes":          s.cfg.Resolver.Notes,
		"apiAvailable":   apiOK,
		"authEnabled":    s.cfg.Token != "",
		"statsAvailable": s.cfg.Stats != nil,
		// mount 是挂载诊断：ok=true 时只是展示路径；false 时前端会渲染成
		// "该往 compose 里加哪一行"的修复指引，并附可复制的配置片段。
		"mount": s.cfg.Resolver.Mount,
		// 前端据此渲染"历史行数"下拉框：超出 tailMax 的请求会被夹紧，
		// 与其让用户选了没效果，不如一开始就不提供这个选项。
		"tailDefault": s.cfg.TailDefault,
		"tailMax":     s.cfg.TailMax,
		"tailBytes":   s.cfg.TailBytes,
	}
	if s.cfg.API != nil {
		resp["dockerSocket"] = s.cfg.API.Socket()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleStats 返回采集器缓存的资源快照，按容器 ID 索引。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Stats == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}

	snap := s.cfg.Stats.Snapshot()
	out := make(map[string]stats.Sample, len(snap))
	for id, v := range snap {
		out[id] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"stats":     out,
	})
}

// ---------------------------------------------------------------- 容器列表

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	list, err := s.list()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"containers": list,
		"total":      len(list),
	})
}

// list 带 2 秒缓存，返回副本以免调用方改到缓存。
func (s *Server) list() ([]containers.Container, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache != nil && time.Since(s.cacheAt) < cacheTTL {
		out := make([]containers.Container, len(s.cache))
		copy(out, s.cache)
		return out, nil
	}

	fresh, err := s.scan.List()
	if err != nil {
		// 扫描失败但手里有旧数据时，宁可给旧数据也别让页面变白。
		if s.cache != nil {
			out := make([]containers.Container, len(s.cache))
			copy(out, s.cache)
			return out, nil
		}
		return nil, err
	}
	s.cache = fresh
	s.cacheAt = time.Now()

	out := make([]containers.Container, len(fresh))
	copy(out, fresh)
	return out, nil
}

// Containers 导出当前容器列表，让资源采集器复用同一份扫描结果，
// 不必自己再读一遍 containers 目录。
func (s *Server) Containers() []containers.Container {
	list, err := s.list()
	if err != nil {
		return nil
	}
	return list
}

// lookup 支持完整 ID、12 位短 ID、容器名，以及无歧义的 ID 前缀。
func (s *Server) lookup(id string) (containers.Container, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return containers.Container{}, errors.New("缺少 container 参数")
	}

	list, err := s.list()
	if err != nil {
		return containers.Container{}, err
	}

	for _, c := range list {
		if c.ID == id || c.Name == id || c.ShortID == id {
			return c, nil
		}
	}

	var hit *containers.Container
	for i := range list {
		if strings.HasPrefix(list[i].ID, id) {
			if hit != nil {
				return containers.Container{}, fmt.Errorf("容器前缀 %s 匹配到多个容器，请写全一点", id)
			}
			hit = &list[i]
		}
	}
	if hit != nil {
		return *hit, nil
	}
	return containers.Container{}, fmt.Errorf("找不到容器 %s", id)
}

// ---------------------------------------------------------------- 一次性拉取

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := s.lookup(q.Get("container"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	// 带 from/to 就是"按时间段查"，走文件定位，跟 tail 行数无关。
	if from, to := parseMs(q.Get("from")), parseMs(q.Get("to")); from > 0 || to > 0 {
		res, err := s.readRange(r.Context(), c, from, to, 0)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, os.ErrNotExist) {
				status = http.StatusNotFound
			}
			writeError(w, status, err)
			return
		}
		writeJSON(w, http.StatusOK, rangePayload(c, res, from, to))
		return
	}

	tail := s.tailCount(q.Get("tail"))

	var entries []logsrc.Entry
	if c.HasLogFile {
		entries, err = logsrc.ReadTail(c.LogFile, tail, s.cfg.TailBytes)
	} else {
		entries, err = s.apiTail(r.Context(), c, tail)
	}
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"container": c.Name,
		"mode":      "tail",
		"entries":   entries,
		"total":     len(entries),
	})
}

// ---------------------------------------------------------------- 时间段

// readRange 取出某段时间内的日志。limit<=0 用默认上限。
func (s *Server) readRange(ctx context.Context, c containers.Container, fromMs, toMs int64, limit int) (*logsrc.RangeResult, error) {
	opt := logsrc.Range{From: nsFromMs(fromMs, false), To: nsFromMs(toMs, true), Limit: limit}

	if c.HasLogFile {
		return logsrc.ReadRange(c.LogFile, opt)
	}

	// 日志不在本地文件里（journald / syslog 等），只能从 docker API 拉一段
	// 再从里面筛，所以早期日志看不到——Note 会把这个限制告诉前端。
	entries, err := s.apiTail(ctx, c, s.cfg.TailMax)
	if err != nil {
		return nil, err
	}

	res := &logsrc.RangeResult{
		Note: fmt.Sprintf("该容器的日志由 %s 驱动管理，只能从 docker API 取最近 %d 行再筛选", c.LogDriver, s.cfg.TailMax),
	}
	for _, e := range entries {
		if opt.From > 0 && e.Ts > 0 && e.Ts < opt.From {
			continue
		}
		if opt.To > 0 && e.Ts > opt.To {
			break
		}
		res.Entries = append(res.Entries, e)
	}
	res.Total = len(res.Entries)
	if res.Total > 0 {
		res.FirstAt = res.Entries[0].TsText
		res.LastAt = res.Entries[res.Total-1].TsText
	}
	return res, nil
}

// rangePayload 统一时段查询的响应结构，SSE 的 meta 事件也用同一份字段。
func rangePayload(c containers.Container, res *logsrc.RangeResult, fromMs, toMs int64) map[string]any {
	return map[string]any{
		"container":  c.Name,
		"mode":       "range",
		"entries":    res.Entries,
		"total":      res.Total,
		"truncated":  res.Truncated,
		"cut":        res.Cut,
		"firstAt":    res.FirstAt,
		"lastAt":     res.LastAt,
		"note":       res.Note,
		"bytes":      res.Bytes,
		"from":       fromMs,
		"to":         toMs,
		"hasLogFile": c.HasLogFile,
	}
}

// handleSpan 回答"这个容器的日志覆盖哪段时间"，只读首行和尾块，很便宜。
// 用户设时间段之前先看一眼，就知道自己要查的那段到底在不在日志里。
func (s *Server) handleSpan(w http.ResponseWriter, r *http.Request) {
	c, err := s.lookup(r.URL.Query().Get("container"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if !c.HasLogFile {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "driver": c.LogDriver})
		return
	}

	first, last, size, err := logsrc.SpanFile(c.LogFile)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": first != "",
		"firstAt":   first,
		"lastAt":    last,
		"size":      size,
	})
}

// apiTail 走 docker API 拉最后 tail 行。
func (s *Server) apiTail(ctx context.Context, c containers.Container, tail int) ([]logsrc.Entry, error) {
	if s.cfg.API == nil {
		return nil, fmt.Errorf("容器 %s 的日志驱动是 %s，日志不在本地文件里；当前未启用 docker socket 兜底", c.Name, c.LogDriver)
	}

	rc, err := s.cfg.API.Logs(ctx, c.ID, tail, false, c.TTY)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), logsrc.MaxScanToken)

	var out []logsrc.Entry
	for sc.Scan() {
		e := logsrc.ParseAPILine(sc.Text())
		if e.Log == "" && e.Ts == 0 {
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------- SSE 实时

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := s.lookup(q.Get("container"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("当前连接不支持流式输出"))
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// nginx 默认会缓冲响应体，不关掉的话日志会攒成一大坨才吐出来。
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	tail := s.tailCount(q.Get("tail"))
	from, to := parseMs(q.Get("from")), parseMs(q.Get("to"))

	// 浏览器自动重连时会带回上次的 id（我们放的是条目纳秒时间戳），
	// 靠它去重就不必担心重连后历史被重复推送一遍。
	var since int64
	if v := strings.TrimSpace(r.Header.Get("Last-Event-ID")); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	}

	if !c.HasLogFile {
		s.streamFromAPI(w, flusher, r, c, tail)
		return
	}

	// 先记下当前文件大小：历史读完之后从这个位置接着推，
	// 中间新写入的内容不会被漏掉，也不会和已发历史重复。
	var start int64
	if st, err := os.Stat(c.LogFile); err == nil {
		start = st.Size()
	}

	if since == 0 {
		if from > 0 || to > 0 {
			// 时间段模式：历史来自"文件里查这个窗口"，而不是"最后 N 行"。
			res, err := s.readRange(r.Context(), c, from, to, 0)
			if err != nil {
				s.sendEventErr(w, flusher, fmt.Errorf("读取日志文件失败: %w", err))
			} else {
				payload := rangePayload(c, res, from, to)
				delete(payload, "entries")
				sendEvent(w, "meta", payload)
				for _, e := range res.Entries {
					sendEntry(w, e)
				}
			}
		} else {
			entries, err := logsrc.ReadTail(c.LogFile, tail, s.cfg.TailBytes)
			if err != nil {
				s.sendEventErr(w, flusher, fmt.Errorf("读取日志文件失败: %w", err))
			} else {
				for _, e := range entries {
					sendEntry(w, e)
				}
			}
		}
		flusher.Flush()
	}

	f := logsrc.NewFollower(c.LogFile)
	f.SetOffset(start)

	ctx := r.Context()
	tick := time.NewTicker(s.cfg.PollInterval)
	defer tick.Stop()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-tick.C:
			moved, err := f.Poll(0, func(e logsrc.Entry) {
				if since > 0 && e.Ts > 0 && e.Ts <= since {
					return
				}
				sendEntry(w, e)
			})
			if err != nil {
				// 单轮读失败不终止连接：容器重建、文件瞬时不可读都会恢复。
				s.cfg.Logger.Printf("跟踪 %s 日志出错: %v", c.Name, err)
				continue
			}
			if moved {
				flusher.Flush()
			}
		}
	}
}

// streamFromAPI 用 docker logs -f 的等价接口做实时推送。
func (s *Server) streamFromAPI(w http.ResponseWriter, flusher http.Flusher, r *http.Request, c containers.Container, tail int) {
	if s.cfg.API == nil {
		s.sendEventErr(w, flusher, fmt.Errorf("容器 %s 的日志驱动是 %s，日志不在本地文件里；当前未启用 docker socket 兜底", c.Name, c.LogDriver))
		return
	}

	rc, err := s.cfg.API.Logs(r.Context(), c.ID, tail, true, c.TTY)
	if err != nil {
		s.sendEventErr(w, flusher, err)
		return
	}
	defer rc.Close()

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), logsrc.MaxScanToken)

	for sc.Scan() {
		e := logsrc.ParseAPILine(sc.Text())
		if e.Log == "" && e.Ts == 0 {
			continue
		}
		sendEntry(w, e)
		flusher.Flush()
	}

	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		s.sendEventErr(w, flusher, err)
	}
}

// ---------------------------------------------------------------- 下载

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	c, err := s.lookup(r.URL.Query().Get("container"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if !c.HasLogFile {
		writeError(w, http.StatusConflict, fmt.Errorf("容器 %s 的日志不在本地文件里，无法直接下载", c.Name))
		return
	}

	f, err := os.Open(c.LogFile)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	name := fmt.Sprintf("%s-%s.log", c.Name, time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// ---------------------------------------------------------------- 工具

// tailCount 解析并夹紧 tail 参数，防止有人伸手要 100 万行。
func (s *Server) tailCount(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return s.cfg.TailDefault
	}
	if n > s.cfg.TailMax {
		return s.cfg.TailMax
	}
	return n
}

// parseMs 解析"Unix 毫秒"时间参数。前端只有毫秒精度，
// 用纳秒会在 JS 的 Number 上丢精度，接口一律收毫秒。
func parseMs(raw string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// nsFromMs 把毫秒换成日志里用的纳秒时间戳。
//
// end 为 true 时取这一毫秒的最后一纳秒：界面上时间是精确到秒的，
// 用户选了 13:48:00 自然是想到 13:48:00.999 为止，不该把这一秒里的日志切掉。
func nsFromMs(ms int64, end bool) int64 {
	if ms <= 0 {
		return 0
	}
	if end {
		return ms*int64(time.Millisecond) + int64(time.Millisecond) - 1
	}
	return ms * int64(time.Millisecond)
}

// sendEvent 写一条自定义事件的 SSE 消息（不带 id，避免影响重连去重）。
func sendEvent(w io.Writer, event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

// sendEntry 写一条 SSE 消息。id 用纳秒时间戳，浏览器重连时会自动带回来。
func sendEntry(w io.Writer, e logsrc.Entry) {
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", e.Ts, data)
}

func (s *Server) sendEventErr(w io.Writer, flusher http.Flusher, err error) {
	data, _ := json.Marshal(map[string]string{"message": err.Error()})
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
	flusher.Flush()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
