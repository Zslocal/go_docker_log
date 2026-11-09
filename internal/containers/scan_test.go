package containers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// writeContainer 在临时目录里造一个容器目录，返回 (containers 根目录, 容器目录)。
func writeContainer(t *testing.T, id, cfgJSON string) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.v2.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, dir
}

func TestScanJSONFileDriver(t *testing.T) {
	cfg := `{
	  "Name": "/web",
	  "Image": "sha256:deadbeef",
	  "Config": {"Image": "nginx:1.27", "Tty": false,
	             "Labels": {"com.docker.compose.project": "shop"}},
	  "State": {"Status": "running", "Running": true,
	            "StartedAt": "2026-11-09T01:00:00.123456789Z",
	            "FinishedAt": "0001-01-01T00:00:00Z"},
	  "HostConfig": {"LogConfig": {"Type": "json-file", "Config": {"max-size": "10m"}}}
	}`
	root, dir := writeContainer(t, testID, cfg)

	logPath := filepath.Join(dir, testID+"-json.log")
	if err := os.WriteFile(logPath, []byte(`{"log":"hi\n","stream":"stdout"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := NewScanner(root, "json-file").List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("容器数量 = %d, 期望 1", len(list))
	}

	c := list[0]
	if c.Name != "web" {
		t.Errorf("Name = %q, 期望 web（前导斜杠应被去掉）", c.Name)
	}
	if c.Image != "nginx:1.27" {
		t.Errorf("Image = %q, 期望 nginx:1.27", c.Image)
	}
	if !c.Running || c.State != "running" {
		t.Errorf("状态解析错误: running=%v state=%q", c.Running, c.State)
	}
	if c.Compose != "shop" {
		t.Errorf("Compose = %q, 期望 shop", c.Compose)
	}
	if !c.HasLogFile {
		t.Fatal("json-file 驱动应当有本地日志文件")
	}
	if !strings.HasSuffix(c.LogFile, testID+"-json.log") {
		t.Errorf("日志路径 = %q, 应以 %s-json.log 结尾", c.LogFile, testID)
	}
	if c.LogSize == 0 {
		t.Errorf("日志大小应当大于 0")
	}
	if c.Finished != "" {
		t.Errorf("零值时间应归一成空串, 得到 %q", c.Finished)
	}
	if c.ShortID != testID[:12] {
		t.Errorf("ShortID = %q, 期望 %q", c.ShortID, testID[:12])
	}
}

func TestScanLocalDriver(t *testing.T) {
	cfg := `{"Name":"/db","Config":{"Image":"postgres:16"},"State":{"Status":"exited","Running":false},
	         "HostConfig":{"LogConfig":{"Type":"local"}}}`
	root, dir := writeContainer(t, testID, cfg)

	localDir := filepath.Join(dir, "local-logs")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "container.log"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := NewScanner(root, "json-file").List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("容器数量 = %d", len(list))
	}
	if !strings.HasSuffix(list[0].LogFile, "local-logs/container.log") &&
		!strings.HasSuffix(list[0].LogFile, `local-logs\container.log`) {
		t.Errorf("local 驱动的日志路径不对: %q", list[0].LogFile)
	}
}

func TestScanUnknownDriverHasNoLogFile(t *testing.T) {
	cfg := `{"Name":"/sys","Config":{"Image":"busybox"},"State":{"Status":"running","Running":true},
	         "HostConfig":{"LogConfig":{"Type":"journald"}}}`
	root, _ := writeContainer(t, testID, cfg)

	list, err := NewScanner(root, "json-file").List()
	if err != nil {
		t.Fatal(err)
	}
	if list[0].HasLogFile {
		t.Errorf("journald 驱动不应有本地日志文件")
	}
	if list[0].LogDriver != "journald" {
		t.Errorf("LogDriver = %q, 期望 journald", list[0].LogDriver)
	}
}

// 容器自己没写 LogConfig 时要落到 daemon.json 的默认驱动上。
func TestScanUsesDefaultDriver(t *testing.T) {
	cfg := `{"Name":"/x","Config":{"Image":"busybox"},"State":{"Running":true}}`
	root, _ := writeContainer(t, testID, cfg)

	list, err := NewScanner(root, "journald").List()
	if err != nil {
		t.Fatal(err)
	}
	if list[0].LogDriver != "journald" {
		t.Errorf("LogDriver = %q, 期望沿用默认值 journald", list[0].LogDriver)
	}
}

// 半写状态 / 损坏的 config.v2.json 应被跳过，而不是让整个列表失败。
func TestScanSkipsBrokenConfig(t *testing.T) {
	root, _ := writeContainer(t, testID, `{ broken`)

	goodID := strings.Repeat("b", 64)
	dir2 := filepath.Join(root, goodID)
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	good := `{"Name":"/ok","Config":{"Image":"redis:7"},"State":{"Status":"running","Running":true}}`
	if err := os.WriteFile(filepath.Join(dir2, "config.v2.json"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := NewScanner(root, "json-file").List()
	if err != nil {
		t.Fatalf("List 不应因单个容器损坏而失败: %v", err)
	}
	if len(list) != 1 || list[0].Name != "ok" {
		t.Fatalf("应当只返回完好的那个容器, 得到 %+v", list)
	}
}

func TestScanSortsRunningFirst(t *testing.T) {
	root := t.TempDir()

	add := func(id, name string, running bool) {
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		status := "exited"
		if running {
			status = "running"
		}
		cfg := `{"Name":"/` + name + `","Config":{"Image":"x"},"State":{"Status":"` + status + `","Running":` +
			map[bool]string{true: "true", false: "false"}[running] + `}}`
		if err := os.WriteFile(filepath.Join(dir, "config.v2.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	add(strings.Repeat("1", 64), "zzz", false)
	add(strings.Repeat("2", 64), "aaa", true)

	list, err := NewScanner(root, "json-file").List()
	if err != nil {
		t.Fatal(err)
	}
	if !list[0].Running {
		t.Errorf("运行中的容器应排在前面, 实际第一项是 %s", list[0].Name)
	}
}

func TestLogFilePathByDriver(t *testing.T) {
	dir := "/data/containers/abc"

	if got := LogFile(dir, "abc", "json-file"); !strings.HasSuffix(got, "/abc-json.log") {
		t.Errorf("json-file 路径错误: %q", got)
	}
	if got := LogFile(dir, "abc", "local"); !strings.HasSuffix(got, "/local-logs/container.log") {
		t.Errorf("local 路径错误: %q", got)
	}
	if got := LogFile(dir, "abc", "syslog"); got != "" {
		t.Errorf("syslog 不应有本地文件, 得到 %q", got)
	}
	if got := LogFile(dir, "abc", ""); !strings.HasSuffix(got, "/abc-json.log") {
		t.Errorf("空驱动应按 json-file 处理, 得到 %q", got)
	}
}

func TestNormalizeTime(t *testing.T) {
	if got := normalizeTime("0001-01-01T00:00:00Z"); got != "" {
		t.Errorf("零值时间应为空串, 得到 %q", got)
	}
	if got := normalizeTime(""); got != "" {
		t.Errorf("空串应为空串, 得到 %q", got)
	}
	if got := normalizeTime("2026-11-09T01:02:03Z"); got == "" {
		t.Errorf("合法时间不应被丢掉")
	}
}
