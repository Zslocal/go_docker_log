package dockerpath

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveExplicitWins(t *testing.T) {
	root := t.TempDir()

	res, err := Resolve(Options{DataRoot: root, SkipCLI: true})
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if res.Source != SourceFlag {
		t.Errorf("来源 = %s, 期望 %s", res.Source, SourceFlag)
	}
	if res.DataRoot != root {
		t.Errorf("DataRoot = %s, 期望 %s", res.DataRoot, root)
	}
}

func TestResolveExplicitMissingFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")

	res, err := Resolve(Options{DataRoot: missing, SkipCLI: true})
	if err == nil {
		t.Fatalf("目录不存在时应当报错")
	}
	if res == nil || len(res.Notes) == 0 {
		t.Fatalf("失败时应当返回带 notes 的 Resolver 供排查")
	}
}

func TestResolveFromDaemonJSON(t *testing.T) {
	root := t.TempDir()
	dj := filepath.Join(t.TempDir(), "daemon.json")

	body := `{"data-root":` + quote(filepath.ToSlash(root)) + `,"log-driver":"local","log-opts":{"max-size":"10m"}}`
	if err := os.WriteFile(dj, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Resolve(Options{DaemonJSON: dj, SkipCLI: true})
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if res.Source != SourceDaemonJSON {
		t.Errorf("来源 = %s, 期望 %s", res.Source, SourceDaemonJSON)
	}
	if res.Daemon.Driver() != "local" {
		t.Errorf("日志驱动 = %s, 期望 local", res.Daemon.Driver())
	}
	if res.Notes[0] == "" {
		t.Errorf("应当记录 daemon.json 的读取结果")
	}
}

// graph 是 Docker 18.03 之前的旧字段名，老机器上还得认。
func TestResolveLegacyGraphField(t *testing.T) {
	root := t.TempDir()
	dj := filepath.Join(t.TempDir(), "daemon.json")
	body := `{"graph":` + quote(filepath.ToSlash(root)) + `}`
	if err := os.WriteFile(dj, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Resolve(Options{DaemonJSON: dj, SkipCLI: true})
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if res.Source != SourceDaemonJSON {
		t.Errorf("来源 = %s, 期望 %s", res.Source, SourceDaemonJSON)
	}
}

func TestResolveInvalidDaemonJSONStillFallsBack(t *testing.T) {
	root := t.TempDir()
	dj := filepath.Join(t.TempDir(), "daemon.json")
	if err := os.WriteFile(dj, []byte(`{ this is not json`), 0o644); err != nil {
		t.Fatal(err)
	}

	// daemon.json 坏了不该直接崩，显式指定的路径仍然可用。
	res, err := Resolve(Options{DataRoot: root, DaemonJSON: dj, SkipCLI: true})
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if res.Source != SourceFlag {
		t.Errorf("来源 = %s, 期望 %s", res.Source, SourceFlag)
	}
}

// socket 报的是守护进程运行时的真实配置，优先级要高于 daemon.json。
func TestResolveSocketInfoBeatsDaemonJSON(t *testing.T) {
	sockRoot := t.TempDir()
	djRoot := t.TempDir()

	dj := filepath.Join(t.TempDir(), "daemon.json")
	body := `{"data-root":` + quote(filepath.ToSlash(djRoot)) + `,"log-driver":"json-file"}`
	if err := os.WriteFile(dj, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Resolve(Options{
		DaemonJSON: dj,
		SkipCLI:    true,
		ProbeInfo: func(context.Context) (SocketInfo, error) {
			return SocketInfo{DataRoot: sockRoot, LogDriver: "local"}, nil
		},
	})
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if res.Source != SourceDockerAPI {
		t.Errorf("来源 = %s, 期望 %s", res.Source, SourceDockerAPI)
	}
	if res.DataRoot != sockRoot {
		t.Errorf("DataRoot = %s, 期望采用 socket 的 %s", res.DataRoot, sockRoot)
	}
	// 运行时的日志驱动同样以 /info 为准
	if res.Daemon.Driver() != "local" {
		t.Errorf("日志驱动 = %s, 期望 local", res.Daemon.Driver())
	}
}

// socket 探不通（没挂、权限不够）时不能卡死，要继续走 daemon.json。
func TestResolveSocketProbeFailureFallsBack(t *testing.T) {
	djRoot := t.TempDir()
	dj := filepath.Join(t.TempDir(), "daemon.json")
	body := `{"data-root":` + quote(filepath.ToSlash(djRoot)) + `}`
	if err := os.WriteFile(dj, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Resolve(Options{
		DaemonJSON: dj,
		SkipCLI:    true,
		ProbeInfo: func(context.Context) (SocketInfo, error) {
			return SocketInfo{}, errors.New("socket 不存在")
		},
	})
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if res.Source != SourceDaemonJSON {
		t.Errorf("来源 = %s, 期望 %s", res.Source, SourceDaemonJSON)
	}
	// daemon.json 里路径必须是正斜杠，比较前统一一下分隔符
	if filepath.ToSlash(res.DataRoot) != filepath.ToSlash(djRoot) {
		t.Errorf("DataRoot = %s, 期望 %s", res.DataRoot, djRoot)
	}
}

// socket 给的宿主机路径在容器里未必挂得上（目录不存在），这种情况要继续往下试。
func TestResolveSocketPathUnreadableFallsThrough(t *testing.T) {
	djRoot := t.TempDir()
	dj := filepath.Join(t.TempDir(), "daemon.json")
	body := `{"data-root":` + quote(filepath.ToSlash(djRoot)) + `}`
	if err := os.WriteFile(dj, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(t.TempDir(), "not-mounted")
	res, err := Resolve(Options{
		DaemonJSON: dj,
		SkipCLI:    true,
		ProbeInfo: func(context.Context) (SocketInfo, error) {
			return SocketInfo{DataRoot: missing}, nil
		},
	})
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if res.Source != SourceDaemonJSON || filepath.ToSlash(res.DataRoot) != filepath.ToSlash(djRoot) {
		t.Errorf("应当回退到 daemon.json，实际来源 %s 路径 %s", res.Source, res.DataRoot)
	}
}

// 容器里部署时，socket 给的是宿主机路径，要能靠 -host-root 映射到挂载点。
func TestResolveSocketPathMappedByHostRoot(t *testing.T) {
	hostRoot := t.TempDir()
	// 模拟宿主机 /app/data 被挂到 <hostRoot>/app/data
	if err := os.MkdirAll(filepath.Join(hostRoot, "app", "data", "containers"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Resolve(Options{
		HostRoot: hostRoot,
		SkipCLI:  true,
		ProbeInfo: func(context.Context) (SocketInfo, error) {
			return SocketInfo{DataRoot: "/app/data"}, nil
		},
	})
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	// DataRoot 保持宿主机语义，方便展示；真正的读取路径由 FS 拼上挂载点
	if res.DataRoot != "/app/data" {
		t.Errorf("DataRoot = %s, 期望保留宿主机路径 /app/data", res.DataRoot)
	}
	want := filepath.Join(hostRoot, "app", "data", "containers")
	if got := filepath.Clean(res.ContainersDir()); got != filepath.Clean(want) {
		t.Errorf("ContainersDir() = %s, 期望 %s", got, want)
	}
}

func TestFSHostRootMapping(t *testing.T) {
	r := &Resolver{HostRoot: "/host"}

	cases := []struct{ in, want string }{
		{"/app/docker/data", "/host/app/docker/data"},
		{"/host/app/docker/data", "/host/app/docker/data"}, // 已带前缀，不能重复加
		{"/host", "/host"},
		{"app/docker/data", "/host/app/docker/data"}, // 缺前导斜杠要补上
	}
	for _, c := range cases {
		if got := r.FS(c.in); got != c.want {
			t.Errorf("FS(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestFSWithoutHostRoot(t *testing.T) {
	r := &Resolver{}
	if got := r.FS("/app/docker/data"); got != "/app/docker/data" {
		t.Errorf("裸机模式下应当原样返回, 得到 %q", got)
	}
}

func TestContainersDir(t *testing.T) {
	r := &Resolver{DataRoot: "/app/docker/data", HostRoot: "/host"}
	want := "/host/app/docker/data/containers"
	if got := r.ContainersDir(); got != want {
		t.Errorf("ContainersDir() = %q, 期望 %q", got, want)
	}
}

func TestNormalizeDataRoot(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/app/docker/data/", "/app/docker/data"},
		{"  /var/lib/docker  ", "/var/lib/docker"},
		{"/", "/"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeDataRoot(c.in); got != c.want {
			t.Errorf("normalizeDataRoot(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeHostRoot(t *testing.T) {
	// "/" 要退化成空串，否则会拼出 //app/docker/data
	if got := normalizeHostRoot("/"); got != "" {
		t.Errorf(`normalizeHostRoot("/") = %q, 期望空串`, got)
	}
	if got := normalizeHostRoot("/host/"); got != "/host" {
		t.Errorf(`normalizeHostRoot("/host/") = %q, 期望 /host`, got)
	}
}

func TestDaemonConfigRootPrecedence(t *testing.T) {
	d := DaemonConfig{DataRoot: "/new", Graph: "/old"}
	got, src := d.Root()
	if got != "/new" || src != SourceDaemonJSON {
		t.Errorf("Root() = (%q, %q), 期望 (/new, %s)", got, src, SourceDaemonJSON)
	}

	if _, src := (DaemonConfig{}).Root(); src != "" {
		t.Errorf("空配置的来源应为空串, 得到 %q", src)
	}
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }
