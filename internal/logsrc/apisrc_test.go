package logsrc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// DockerAPI 的 /info 解析。用真实 HTTP 服务顶替守护进程，
// socket 走 tcp:// 注入（生产是 unix，但解析逻辑同一条路径）。
func TestDockerAPIInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// 混入大量无关字段，确认我们只取需要的两个
		_, _ = w.Write([]byte(`{
			"ID":"abc","Containers":18,"Driver":"overlay2",
			"DockerRootDir":"/app/docker/data",
			"LoggingDriver":"json-file",
			"Plugins":{"Volume":["local"]}
		}`))
	}))
	defer srv.Close()

	api := NewDockerAPI("tcp://" + strings.TrimPrefix(srv.URL, "http://"))

	info, err := api.Info(context.Background())
	if err != nil {
		t.Fatalf("Info 失败: %v", err)
	}
	if info.DockerRootDir != "/app/docker/data" {
		t.Errorf("DockerRootDir = %q", info.DockerRootDir)
	}
	if info.LoggingDriver != "json-file" {
		t.Errorf("LoggingDriver = %q", info.LoggingDriver)
	}
}

// 没有 DockerRootDir 的响应要报错，否则调用方会拿着空路径去拼目录。
func TestDockerAPIInfoMissingRootDir(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ID":"abc","LoggingDriver":"local"}`))
	}))
	defer srv.Close()

	api := NewDockerAPI("tcp://" + strings.TrimPrefix(srv.URL, "http://"))

	if _, err := api.Info(context.Background()); err == nil {
		t.Fatal("缺少 DockerRootDir 时应当报错")
	}
}

func TestDockerAPIInfoServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer srv.Close()

	api := NewDockerAPI("tcp://" + strings.TrimPrefix(srv.URL, "http://"))

	_, err := api.Info(context.Background())
	if err == nil {
		t.Fatal("非 200 应当报错")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误信息应包含状态码, 实际 %v", err)
	}
}
