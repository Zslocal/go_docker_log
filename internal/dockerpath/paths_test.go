package dockerpath

import (
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
