package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zslocal/go_docker_log/internal/dockerpath"
)

const testID = "1111222233334444555566667777888899990000111122223333444455556666"

// newTestServer 造一个带两个容器的临时 data-root，并起一个 httptest 服务。
func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()

	root := t.TempDir()
	cdir := filepath.Join(root, "containers")

	// 容器一：json-file 驱动，带 3 行日志
	dir1 := filepath.Join(cdir, testID)
	if err := os.MkdirAll(dir1, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg1 := `{"Name":"/api","Config":{"Image":"app:1.0"},"State":{"Status":"running","Running":true},
	          "HostConfig":{"LogConfig":{"Type":"json-file"}}}`
	if err := os.WriteFile(filepath.Join(dir1, "config.v2.json"), []byte(cfg1), 0o644); err != nil {
		t.Fatal(err)
	}
	logs := strings.Join([]string{
		`{"log":"boot ok\n","stream":"stdout","time":"2026-11-09T01:00:00.000000001Z"}`,
		`{"log":"connection refused\n","stream":"stderr","time":"2026-11-09T01:00:01.000000002Z"}`,
		`{"log":"retrying\n","stream":"stdout","time":"2026-11-09T01:00:02.000000003Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir1, testID+"-json.log"), []byte(logs), 0o644); err != nil {
		t.Fatal(err)
	}

	// 容器二：journald 驱动，没有本地文件
	dir2 := filepath.Join(cdir, strings.Repeat("a", 64))
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg2 := `{"Name":"/sys","Config":{"Image":"busybox"},"State":{"Status":"exited","Running":false},
	          "HostConfig":{"LogConfig":{"Type":"journald"}}}`
	if err := os.WriteFile(filepath.Join(dir2, "config.v2.json"), []byte(cfg2), 0o644); err != nil {
		t.Fatal(err)
	}

	res := &dockerpath.Resolver{DataRoot: root, Source: dockerpath.SourceFlag}
	srv := New(Config{
		Resolver:     res,
		TailDefault:  100,
		PollInterval: 50 * time.Millisecond,
	})

	ts := httptest.NewServer(srv.Routes(nil))
	t.Cleanup(ts.Close)
	return ts, testID
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
	}
	return resp.StatusCode
}

func TestContainersEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)

	var body struct {
		Containers []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Running    bool   `json:"running"`
			HasLogFile bool   `json:"hasLogFile"`
			LogSize    int64  `json:"logSize"`
		} `json:"containers"`
		Total int `json:"total"`
	}
	if code := getJSON(t, ts.URL+"/api/containers", &body); code != 200 {
		t.Fatalf("状态码 = %d", code)
	}
	if body.Total != 2 {
		t.Fatalf("容器数 = %d, 期望 2", body.Total)
	}
	// 运行中的排前面
	if body.Containers[0].Name != "api" {
		t.Errorf("第一个容器 = %q, 期望 api", body.Containers[0].Name)
	}
	if !body.Containers[0].HasLogFile || body.Containers[0].LogSize == 0 {
		t.Errorf("api 容器应当有非空日志文件")
	}
}

func TestLogsEndpoint(t *testing.T) {
	ts, id := newTestServer(t)

	var body struct {
		Container string `json:"container"`
		Total     int    `json:"total"`
		Entries   []struct {
			Ts     int64  `json:"ts"`
			Stream string `json:"stream"`
			Log    string `json:"log"`
		} `json:"entries"`
	}
	if code := getJSON(t, ts.URL+"/api/logs?container="+id+"&tail=10", &body); code != 200 {
		t.Fatalf("状态码 = %d", code)
	}
	if body.Total != 3 {
		t.Fatalf("条数 = %d, 期望 3", body.Total)
	}
	if body.Entries[1].Stream != "stderr" {
		t.Errorf("第二条 stream = %q, 期望 stderr", body.Entries[1].Stream)
	}
	if body.Entries[0].Ts == 0 {
		t.Errorf("时间戳应被解析")
	}
}

// 短 ID 和容器名都要能定位到同一个容器。
func TestLookupByShortIDAndName(t *testing.T) {
	ts, id := newTestServer(t)

	for _, key := range []string{id[:12], "api", id} {
		var body struct{ Total int }
		if code := getJSON(t, ts.URL+"/api/logs?container="+key, &body); code != 200 {
			t.Errorf("用 %q 查询返回 %d", key, code)
		}
		if body.Total != 3 {
			t.Errorf("用 %q 查询得到 %d 条", key, body.Total)
		}
	}
}

func TestLogsUnknownContainer(t *testing.T) {
	ts, _ := newTestServer(t)
	if code := getJSON(t, ts.URL+"/api/logs?container=does-not-exist", nil); code != http.StatusNotFound {
		t.Errorf("状态码 = %d, 期望 404", code)
	}
}

// journald 容器没有本地文件，未启用 API 时应给出明确的 500 与提示信息。
func TestLogsNeedsAPIWhenNoLocalFile(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/logs?container=sys")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, 期望 500", resp.StatusCode)
	}
	var e struct{ Error string }
	json.NewDecoder(resp.Body).Decode(&e)
	if !strings.Contains(e.Error, "journald") {
		t.Errorf("错误信息应说明日志驱动问题, 实际 %q", e.Error)
	}
}

// SSE 要先吐历史，再挂住推实时。
func TestStreamEndpointSendsHistory(t *testing.T) {
	ts, id := newTestServer(t)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/stream?container="+id+"&tail=10", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	var dataLines []string
	for sc.Scan() && len(dataLines) < 3 {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}

	if len(dataLines) < 3 {
		t.Fatalf("只收到 %d 条历史日志", len(dataLines))
	}
	var first struct {
		Log    string `json:"log"`
		Stream string `json:"stream"`
	}
	if err := json.Unmarshal([]byte(dataLines[0]), &first); err != nil {
		t.Fatalf("解析首条失败: %v", err)
	}
	if first.Log != "boot ok\n" {
		t.Errorf("首条 = %q", first.Log)
	}
}

func TestStreamUnknownContainerIs404(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/stream?container=nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("状态码 = %d, 期望 404", resp.StatusCode)
	}
}

func TestDownloadEndpoint(t *testing.T) {
	ts, id := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/download?container=" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

func TestMetaEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)

	var m struct {
		DataRoot    string   `json:"dataRoot"`
		Source      string   `json:"source"`
		Notes       []string `json:"notes"`
		TailDefault int      `json:"tailDefault"`
		TailMax     int      `json:"tailMax"`
		TailBytes   int64    `json:"tailBytes"`
	}
	if code := getJSON(t, ts.URL+"/api/meta", &m); code != 200 {
		t.Fatalf("状态码 = %d", code)
	}
	if m.DataRoot == "" || m.Source != "flag" {
		t.Errorf("元信息不完整: %+v", m)
	}
	// 前端靠这三个值渲染"历史行数"下拉框，缺了会退化成写死的默认值。
	if m.TailDefault <= 0 || m.TailMax < m.TailDefault || m.TailBytes <= 0 {
		t.Errorf("tail 配置不合法: default=%d max=%d bytes=%d", m.TailDefault, m.TailMax, m.TailBytes)
	}
}

// 定位失败时服务仍要能起来：页面拿到的是"该怎么挂"的建议 + 空容器列表，
// 而不是 500。这是容器化部署第一次启动的默认路径，必须友好。
func TestMetaExposesMountDiagnosis(t *testing.T) {
	res := &dockerpath.Resolver{
		Mount: dockerpath.MountCheck{
			Kind:          dockerpath.MountNotMount,
			HostPath:      "/app/docker/data",
			ContainerPath: "/host/app/docker/data",
			Source:        dockerpath.SourceDockerAPI,
			Message:       "守护进程报告宿主机 data-root 是 /app/docker/data，但容器里读不到",
			Volume:        "/app/docker/data:/host/app/docker/data:ro",
			RunArg:        "-v /app/docker/data:/host/app/docker/data:ro",
			Restart:       "docker compose up -d",
		},
	}
	srv := New(Config{Resolver: res, TailDefault: 100})
	ts := httptest.NewServer(srv.Routes(nil))
	defer ts.Close()

	var m struct {
		DataRoot string `json:"dataRoot"`
		Mount    struct {
			OK     bool   `json:"ok"`
			Kind   string `json:"kind"`
			Volume string `json:"volume"`
		} `json:"mount"`
	}
	if code := getJSON(t, ts.URL+"/api/meta", &m); code != 200 {
		t.Fatalf("状态码 = %d", code)
	}
	if m.Mount.OK {
		t.Error("mount.ok 应为 false")
	}
	if m.Mount.Kind != "not-mounted" {
		t.Errorf("mount.kind = %q, 期望 not-mounted", m.Mount.Kind)
	}
	if !strings.Contains(m.Mount.Volume, "/host/app/docker/data:ro") {
		t.Errorf("mount.volume = %q", m.Mount.Volume)
	}

	// data-root 为空时容器列表必须是 200 + 空数组
	var list struct {
		Containers []struct{} `json:"containers"`
		Total      int        `json:"total"`
	}
	if code := getJSON(t, ts.URL+"/api/containers", &list); code != 200 {
		t.Fatalf("容器列表状态码 = %d，诊断模式不应报错", code)
	}
	if list.Total != 0 {
		t.Errorf("total = %d, 期望 0", list.Total)
	}
}

// ---------------------------------------------------------------- 时间段查询

// 测试日志的三条时间分别是 01:00:00 / 01:00:01 / 01:00:02 (UTC)。
func msAt(sec int) int64 {
	base := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(sec) * time.Second).UnixMilli()
}

// 时间段查询不能受 tail 行数限制：只给 tail=1 也要能拿到窗口内的全部 2 条。
func TestLogsByTimeRangeIgnoresTail(t *testing.T) {
	ts, id := newTestServer(t)

	url := fmt.Sprintf("%s/api/logs?container=%s&tail=1&from=%d&to=%d", ts.URL, id, msAt(0), msAt(1))
	var body struct {
		Mode      string `json:"mode"`
		Total     int    `json:"total"`
		Truncated bool   `json:"truncated"`
		FirstAt   string `json:"firstAt"`
		LastAt    string `json:"lastAt"`
		Entries   []struct {
			Log string `json:"log"`
		} `json:"entries"`
	}
	if code := getJSON(t, url, &body); code != 200 {
		t.Fatalf("状态码 = %d", code)
	}
	if body.Mode != "range" {
		t.Errorf("mode = %q, 期望 range", body.Mode)
	}
	if body.Total != 2 || len(body.Entries) != 2 {
		t.Fatalf("条数 = %d/%d, 期望 2（tail=1 不应限制时段查询）", body.Total, len(body.Entries))
	}
	if body.Entries[0].Log != "boot ok\n" {
		t.Errorf("首条 = %q", body.Entries[0].Log)
	}
	// 空结果时前端靠它告诉用户"日志里其实只有哪一段"
	if body.FirstAt == "" || body.LastAt == "" {
		t.Errorf("应返回文件时间跨度: %+v", body)
	}
}

func TestLogsByTimeRangeEmpty(t *testing.T) {
	ts, id := newTestServer(t)

	url := fmt.Sprintf("%s/api/logs?container=%s&from=%d&to=%d", ts.URL, id, msAt(3600), msAt(7200))
	var body struct {
		Total   int        `json:"total"`
		Entries []struct{} `json:"entries"`
	}
	if code := getJSON(t, url, &body); code != 200 {
		t.Fatalf("状态码 = %d", code)
	}
	if body.Total != 0 || len(body.Entries) != 0 {
		t.Errorf("应当为空结果, 实际 %d", body.Total)
	}
}

func TestSpanEndpoint(t *testing.T) {
	ts, id := newTestServer(t)

	var body struct {
		Available bool   `json:"available"`
		FirstAt   string `json:"firstAt"`
		LastAt    string `json:"lastAt"`
		Size      int64  `json:"size"`
	}
	if code := getJSON(t, ts.URL+"/api/span?container="+id, &body); code != 200 {
		t.Fatalf("状态码 = %d", code)
	}
	if !body.Available || body.FirstAt == "" || body.Size <= 0 {
		t.Fatalf("span 结果不完整: %+v", body)
	}
	if !strings.HasPrefix(body.FirstAt, "2026-11-09T01:00:00") {
		t.Errorf("firstAt = %q", body.FirstAt)
	}
}

// journald 容器没有本地文件，span 应明确回报不可用而不是报错。
func TestSpanEndpointNoLocalFile(t *testing.T) {
	ts, _ := newTestServer(t)

	var body struct {
		Available bool   `json:"available"`
		Driver    string `json:"driver"`
	}
	if code := getJSON(t, ts.URL+"/api/span?container=sys", &body); code != 200 {
		t.Fatalf("状态码 = %d", code)
	}
	if body.Available || body.Driver != "journald" {
		t.Errorf("journald 容器应报告不可用: %+v", body)
	}
}

// 实时流带 from 时应先推一条 meta，再推该时间段的历史。
func TestStreamWithRangeSendsMetaFirst(t *testing.T) {
	ts, id := newTestServer(t)

	url := fmt.Sprintf("%s/api/stream?container=%s&from=%d", ts.URL, id, msAt(1))
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	var meta string
	var logs []string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			if meta == "" {
				meta = strings.TrimPrefix(line, "data: ")
			} else {
				logs = append(logs, strings.TrimPrefix(line, "data: "))
			}
		}
		if len(logs) >= 2 {
			break
		}
	}

	var m struct {
		Mode      string `json:"mode"`
		Total     int    `json:"total"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		t.Fatalf("meta 帧解析失败: %v (%q)", err, meta)
	}
	if m.Mode != "range" || m.Total != 2 {
		t.Errorf("meta = %+v, 期望 mode=range total=2", m)
	}
	if len(logs) != 2 {
		t.Fatalf("历史条数 = %d, 期望 2", len(logs))
	}
}
