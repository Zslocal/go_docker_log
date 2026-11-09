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

// ---------------------------------------------------------------- 挂载诊断

// socket 报告了宿主机路径，但容器里没挂上——这是容器化部署最常见的失败。
// 此时必须给出可直接粘贴的 volumes 行，而不是一句 not found。
func TestMountHintWhenSocketPathNotMounted(t *testing.T) {
	hostRoot := t.TempDir()
	missingDJ := filepath.Join(t.TempDir(), "nope.json")

	res, err := Resolve(Options{
		HostRoot:   hostRoot,
		DaemonJSON: missingDJ,
		SkipCLI:    true,
		ProbeInfo: func(context.Context) (SocketInfo, error) {
			return SocketInfo{DataRoot: "/app/docker/data", LogDriver: "json-file"}, nil
		},
	})
	if err == nil {
		t.Fatal("目录读不到时应当返回错误，让调用方知道还没配好")
	}

	m := res.Mount
	if m.OK {
		t.Error("Mount.OK 应当为 false")
	}
	if m.Kind != MountNotMount {
		t.Errorf("Kind = %s, 期望 %s", m.Kind, MountNotMount)
	}
	if m.HostPath != "/app/docker/data" {
		t.Errorf("HostPath = %q, 期望 /app/docker/data", m.HostPath)
	}

	wantContainer := filepath.ToSlash(filepath.Join(hostRoot, "app", "docker", "data"))
	if got := filepath.ToSlash(m.ContainerPath); got != wantContainer {
		t.Errorf("ContainerPath = %q, 期望 %q", got, wantContainer)
	}
	wantVolume := "/app/docker/data:" + m.ContainerPath + ":ro"
	if m.Volume != wantVolume {
		t.Errorf("Volume = %q, 期望 %q", m.Volume, wantVolume)
	}
	if m.RunArg != "-v "+m.Volume {
		t.Errorf("RunArg = %q 与 Volume 不匹配", m.RunArg)
	}
	// 改挂载必须重建容器，这句提示不能少
	if !strings.Contains(m.Restart, "docker compose up -d") {
		t.Errorf("Restart 应给出重建容器的命令, 实际 %q", m.Restart)
	}
}

// 连 socket 都没挂的时候，猜出来的路径不能当事实：先教用户挂 socket。
func TestMountNoSocketGivesSocketHint(t *testing.T) {
	res, err := Resolve(Options{
		HostRoot:   t.TempDir(),
		DaemonJSON: filepath.Join(t.TempDir(), "nope.json"),
		SkipCLI:    true,
		// ProbeInfo 为 nil == 没挂 socket / 用了 -no-api
	})
	if err == nil {
		t.Fatal("什么都没探测到时应当返回错误")
	}

	m := res.Mount
	if m.Kind != MountNoSocket {
		t.Errorf("Kind = %s, 期望 %s", m.Kind, MountNoSocket)
	}
	if m.SocketVolume != "/var/run/docker.sock:/var/run/docker.sock:ro" {
		t.Errorf("SocketVolume = %q，应提示挂 socket", m.SocketVolume)
	}
	// 猜的是默认路径，但也要给出可对照的挂载行
	if !strings.Contains(m.Volume, "/var/lib/docker:") {
		t.Errorf("Volume = %q，应包含默认路径的挂法", m.Volume)
	}
}

// 目录挂出来了但里面没有 containers/：多半是挂到了错误的位置。
func TestMountDetectsWrongDirectory(t *testing.T) {
	hostRoot := t.TempDir()
	// 宿主机 /app/data 存在（docker 可能自动建了空目录），但没有 containers/
	if err := os.MkdirAll(filepath.Join(hostRoot, "app", "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Resolve(Options{
		HostRoot:   hostRoot,
		DaemonJSON: filepath.Join(t.TempDir(), "nope.json"),
		SkipCLI:    true,
		ProbeInfo: func(context.Context) (SocketInfo, error) {
			return SocketInfo{DataRoot: "/app/data"}, nil
		},
	})
	if err != nil {
		t.Fatalf("目录存在时不应报错（页面要能渲染出诊断提示）: %v", err)
	}
	if res.Mount.Kind != MountWrongDir {
		t.Errorf("Kind = %s, 期望 %s", res.Mount.Kind, MountWrongDir)
	}
	if res.Mount.OK {
		t.Error("缺 containers/ 时 Mount.OK 应当为 false")
	}
	if res.DataRoot != "/app/data" {
		t.Errorf("DataRoot = %s, 期望仍然采用探测到的 /app/data", res.DataRoot)
	}
}

// 正常情况：挂载正确、结构完整，不该出现任何修复提示。
func TestMountOKNoHint(t *testing.T) {
	hostRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostRoot, "app", "docker", "data", "containers"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Resolve(Options{
		HostRoot:   hostRoot,
		DaemonJSON: filepath.Join(t.TempDir(), "nope.json"),
		SkipCLI:    true,
		ProbeInfo: func(context.Context) (SocketInfo, error) {
			return SocketInfo{DataRoot: "/app/docker/data"}, nil
		},
	})
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if !res.Mount.OK || res.Mount.Kind != MountOK {
		t.Errorf("应当判定为正常: ok=%v kind=%s msg=%s", res.Mount.OK, res.Mount.Kind, res.Mount.Message)
	}
	if res.Mount.Volume != "" || res.Mount.Restart != "" {
		t.Error("正常时不该给出挂载建议")
	}
}

// 裸机运行不需要挂载，找不到就是找不到——不能给出无意义的 -v。
func TestMountNotFoundBareMetal(t *testing.T) {
	res, err := Resolve(Options{
		DaemonJSON: filepath.Join(t.TempDir(), "nope.json"),
		SkipCLI:    true,
	})
	if err == nil {
		t.Fatal("应当返回错误")
	}
	if res.Mount.Kind != MountNotFound {
		t.Errorf("Kind = %s, 期望 %s", res.Mount.Kind, MountNotFound)
	}
	if res.Mount.Volume != "" || res.Mount.RunArg != "" {
		t.Errorf("裸机不该给出挂载建议，实际 volume=%q run=%q", res.Mount.Volume, res.Mount.RunArg)
	}
	// 提示里要说明真正的原因：权限 / 守护进程没起
	if !strings.Contains(res.Mount.Message, "root") {
		t.Errorf("Message 应提示以 root 运行, 实际 %q", res.Mount.Message)
	}
}
