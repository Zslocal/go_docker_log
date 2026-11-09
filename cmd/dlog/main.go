// Command dlog 是一个只读本地日志文件的 Docker 日志查看器。
//
// 它不依赖 docker CLI，也不强制依赖 docker.sock：定位到 Docker 的 data-root 之后，
// 直接读 containers/<id>/<id>-json.log 并用 SSE 推到浏览器。
// 只有当日志驱动不是 json-file/local 时，才回退到 docker socket 取日志；
// 容器资源占用（CPU/内存）同样走 socket，没挂 socket 就不显示。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/Zslocal/go_docker_log/internal/containers"
	"github.com/Zslocal/go_docker_log/internal/dockerpath"
	"github.com/Zslocal/go_docker_log/internal/logsrc"
	"github.com/Zslocal/go_docker_log/internal/server"
	"github.com/Zslocal/go_docker_log/internal/stats"
	"github.com/Zslocal/go_docker_log/internal/web"
)

// version 由构建脚本通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	var (
		listen     = flag.String("listen", ":8080", "HTTP 监听地址")
		dataRoot   = flag.String("data-root", "", "Docker 数据根目录（留空则自动探测）")
		hostRoot   = flag.String("host-root", "", "容器内部署时宿主机根目录的挂载点，例如 /host")
		daemonJSON = flag.String("daemon-json", "", "daemon.json 路径（留空用 /etc/docker/daemon.json）")
		dockerSock = flag.String("docker-sock", "/var/run/docker.sock", "docker socket 路径，日志兜底与资源采集用")
		webToken   = flag.String("web-token", "", "访问口令；非空时打开页面需先输入口令（也接受 ?token= 或 Bearer）")
		tailMax    = flag.Int("tail-max", 5000, "单次最多返回多少条历史日志")
		tailBytes  = flag.Int64("tail-bytes", 16<<20, "回溯历史日志时最多扫描的字节数")
		pollMS     = flag.Int("poll-ms", 250, "日志文件轮询间隔（毫秒）")
		statsMS    = flag.Int("stats-ms", 2000, "容器资源采集间隔（毫秒）")
		noCLI      = flag.Bool("no-cli", false, "不调用 docker 命令探测 data-root")
		noAPI      = flag.Bool("no-api", false, "禁用 docker socket，只读本地日志文件，也不采集资源占用")
		noStats    = flag.Bool("no-stats", false, "不采集容器 CPU/内存占用")
		showVer    = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("dlog %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)

	// ---- 1. docker socket 通道（可选）----
	// 先建立客户端，data-root 探测就能直接问守护进程要权威配置；
	// 后面日志兜底和资源采集复用同一个实例。
	var api *logsrc.DockerAPI
	if !*noAPI {
		api = logsrc.NewDockerAPI(*dockerSock)
	}

	// ---- 2. 定位 Docker 数据目录 ----
	probe := dockerpath.Options{
		DataRoot:   *dataRoot,
		HostRoot:   *hostRoot,
		DaemonJSON: *daemonJSON,
		SkipCLI:    *noCLI,
		Timeout:    2 * time.Second,
	}
	if api != nil {
		probe.ProbeInfo = func(ctx context.Context) (dockerpath.SocketInfo, error) {
			info, err := api.Info(ctx)
			if err != nil {
				return dockerpath.SocketInfo{}, err
			}
			return dockerpath.SocketInfo{DataRoot: info.DockerRootDir, LogDriver: info.LoggingDriver}, nil
		}
	}

	res, err := dockerpath.Resolve(probe)
	if err != nil {
		logger.Printf("定位 Docker 数据目录失败：%v", err)
		for _, n := range res.Notes {
			logger.Printf("  · %s", n)
		}
		os.Exit(1)
	}
	for _, n := range res.Notes {
		logger.Printf("探测：%s", n)
	}

	// ---- 3. 检查 socket 是否真的可用（决定要不要提示降级）----
	if api != nil {
		if err := api.Available(2 * time.Second); err != nil {
			// 不致命：socket 可能晚一点才就绪，而且 json-file 的容器根本用不到它。
			logger.Printf("提示：docker socket 暂不可用（%v），日志驱动非 json-file 的容器不可读，也不会显示资源占用", err)
		}
	} else {
		logger.Printf("提示：已禁用 docker socket（-no-api），仅按 daemon.json / 默认路径定位")
	}

	// ---- 4. 组装服务 ----
	static, err := web.Handler()
	if err != nil {
		logger.Fatalf("加载内嵌前端失败：%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 采集器要反过来读 server 的容器列表，用闭包延迟绑定，避免循环依赖。
	var srv *server.Server

	var statsCol *stats.Collector
	if api != nil && !*noStats {
		statsCol = stats.New(api, func() []containers.Container {
			if srv == nil {
				return nil
			}
			return srv.Containers()
		}, time.Duration(*statsMS)*time.Millisecond, logger)
	}

	srv = server.New(server.Config{
		Resolver:     res,
		API:          api,
		Stats:        statsCol,
		Token:        *webToken,
		TailMax:      *tailMax,
		TailBytes:    *tailBytes,
		PollInterval: time.Duration(*pollMS) * time.Millisecond,
		Logger:       logger,
	})

	if statsCol != nil {
		go statsCol.Run(ctx)
	}

	httpSrv := &http.Server{
		Addr:    *listen,
		Handler: srv.Routes(static),
		// SSE 是长连接，WriteTimeout 必须留 0，否则推送会被硬掐断。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Printf("dlog %s 启动，监听 %s", version, *listen)
		logger.Printf("数据目录 %s（来源 %s），容器目录 %s", res.DataRoot, res.Source, res.ContainersDir())

		switch {
		case *webToken == "":
			logger.Printf("警告：未设置 -web-token，任何能访问此端口的人都能看日志")
		case len(*webToken) < 8:
			logger.Printf("警告：访问口令长度仅 %d 位，建议至少 16 位随机字符", len(*webToken))
		default:
			logger.Printf("访问口令已启用")
		}
		if statsCol == nil {
			logger.Printf("资源占用采集未启用（需要 docker socket）")
		}

		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("HTTP 服务异常退出：%v", err)
		}
	}()

	<-ctx.Done()
	logger.Printf("收到退出信号，正在关闭…")

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		logger.Printf("关闭超时：%v", err)
	}
}
