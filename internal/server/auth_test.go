package server

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zslocal/go_docker_log/internal/dockerpath"
)

const testToken = "s3cr3t-token-for-test"

// newAuthedServer 起一个带访问口令的服务，里面放一个最小可用的容器。
func newAuthedServer(t *testing.T, token string) *httptest.Server {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "containers", testID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"Name":"/api","Config":{"Image":"app:1.0"},"State":{"Status":"running","Running":true}}`
	if err := os.WriteFile(filepath.Join(dir, "config.v2.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, testID+"-json.log"),
		[]byte(`{"log":"hi\n","stream":"stdout","time":"2026-11-09T01:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := &dockerpath.Resolver{DataRoot: root, Source: dockerpath.SourceFlag}
	srv := New(Config{Resolver: res, Token: token, TailDefault: 10})

	ts := httptest.NewServer(srv.Routes(nil))
	t.Cleanup(ts.Close)
	return ts
}

func postLogin(t *testing.T, client *http.Client, base, body string) *http.Response {
	t.Helper()
	resp, err := client.Post(base+"/api/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("登录请求失败: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// 没配口令时一切照旧，不能被鉴权逻辑影响。
func TestAuthDisabledWhenNoToken(t *testing.T) {
	ts := newAuthedServer(t, "")

	resp, err := http.Get(ts.URL + "/api/containers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d, 期望 200", resp.StatusCode)
	}
}

func TestAuthBlocksAnonymousAPI(t *testing.T) {
	ts := newAuthedServer(t, testToken)

	resp, err := http.Get(ts.URL + "/api/containers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", resp.StatusCode)
	}
	var e struct{ Error string }
	json.NewDecoder(resp.Body).Decode(&e)
	if e.Error == "" {
		t.Errorf("未授权时应返回 JSON 错误说明")
	}
}

// 浏览器打开首页时应拿到登录页而不是一段 JSON。
func TestAuthServesLoginPage(t *testing.T) {
	ts := newAuthedServer(t, testToken)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, 期望 text/html", ct)
	}

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "访问口令") {
		t.Errorf("返回内容不像登录页")
	}
}

func TestAuthLoginFlow(t *testing.T) {
	ts := newAuthedServer(t, testToken)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	// 1. 错误口令
	bad := postLogin(t, client, ts.URL, `{"token":"nope"}`)
	if bad.StatusCode != http.StatusUnauthorized {
		t.Errorf("错误口令状态码 = %d, 期望 401", bad.StatusCode)
	}

	// 2. 正确口令
	ok := postLogin(t, client, ts.URL, `{"token":"`+testToken+`"}`)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("正确口令状态码 = %d, 期望 200", ok.StatusCode)
	}

	// 3. 带上 Cookie 再访问
	resp, err := client.Get(ts.URL + "/api/containers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("登录后状态码 = %d, 期望 200", resp.StatusCode)
	}
}

// ?token= 适合临时分享一条链接，访问后应当顺手种下 Cookie。
func TestAuthQueryToken(t *testing.T) {
	ts := newAuthedServer(t, testToken)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp, err := client.Get(ts.URL + "/api/containers?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
	}

	// Cookie 已种下，去掉 token 参数仍可访问
	resp2, err := client.Get(ts.URL + "/api/containers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("第二次访问状态码 = %d, 期望 200", resp2.StatusCode)
	}
}

func TestAuthBearerToken(t *testing.T) {
	ts := newAuthedServer(t, testToken)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/containers", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d, 期望 200", resp.StatusCode)
	}
}

func TestAuthQueryTokenWrong(t *testing.T) {
	ts := newAuthedServer(t, testToken)

	resp, err := http.Get(ts.URL + "/api/containers?token=guess")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("状态码 = %d, 期望 401", resp.StatusCode)
	}
}

// 伪造的 Cookie 不能放行（Cookie 里存的是口令派生值，不是明文）。
func TestAuthForgedCookieRejected(t *testing.T) {
	ts := newAuthedServer(t, testToken)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/containers", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: testToken})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("把口令本身当 Cookie 用时状态码 = %d, 期望 401", resp.StatusCode)
	}
}

func TestAuthLogout(t *testing.T) {
	ts := newAuthedServer(t, testToken)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	if resp := postLogin(t, client, ts.URL, `{"token":"`+testToken+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("登录失败: %d", resp.StatusCode)
	}

	resp, err := client.Get(ts.URL + "/api/logout")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp2, err := client.Get(ts.URL + "/api/containers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("登出后状态码 = %d, 期望 401", resp2.StatusCode)
	}
}

// meta 应当如实反映鉴权与采集开关，前端据此决定要不要轮询 stats。
func TestMetaReportsFeatures(t *testing.T) {
	ts := newAuthedServer(t, "")

	var m struct {
		AuthEnabled    bool `json:"authEnabled"`
		StatsAvailable bool `json:"statsAvailable"`
	}
	if code := getJSON(t, ts.URL+"/api/meta", &m); code != 200 {
		t.Fatalf("状态码 = %d", code)
	}
	if m.AuthEnabled {
		t.Errorf("未配置口令时 authEnabled 应为 false")
	}
	if m.StatsAvailable {
		t.Errorf("未注入采集器时 statsAvailable 应为 false")
	}
}

// 没注入采集器时 /api/stats 要明确告诉前端不可用，而不是返回空对象。
func TestStatsUnavailable(t *testing.T) {
	ts := newAuthedServer(t, "")

	var body struct {
		Available bool `json:"available"`
	}
	if code := getJSON(t, ts.URL+"/api/stats", &body); code != 200 {
		t.Fatalf("状态码 = %d", code)
	}
	if body.Available {
		t.Errorf("没有采集器时 available 应为 false")
	}
}
