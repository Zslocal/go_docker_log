package stats

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zslocal/go_docker_log/internal/containers"
	"github.com/Zslocal/go_docker_log/internal/logsrc"
)

func TestCPUPercent(t *testing.T) {
	// 宿主机 1 秒内涨了 2e9 纳秒（2 核 × 1e9），容器涨了 0.5e9 → 用满半个核 = 50%
	prev := cpuPoint{total: 1_000_000_000, system: 100_000_000_000}
	cur := cpuPoint{total: 1_500_000_000, system: 102_000_000_000}

	got := cpuPercent(prev, cur, 2)
	if got < 49.9 || got > 50.1 {
		t.Errorf("CPU%% = %.2f, 期望 ~50", got)
	}
}

// 4 核跑满应该是 400%，与 docker stats 一致。
// 系统 CPU 时间在同样的 1 秒里涨 4e9（4 核 × 1e9），容器也涨 4e9。
func TestCPUPercentMultiCore(t *testing.T) {
	prev := cpuPoint{total: 0, system: 1_000_000_000}
	cur := cpuPoint{total: 4_000_000_000, system: 5_000_000_000}

	got := cpuPercent(prev, cur, 4)
	if got < 399 || got > 401 {
		t.Errorf("CPU%% = %.2f, 期望 ~400", got)
	}
}

// 系统计数器回绕或容器刚重启时不能算出负数 / NaN。
func TestCPUPercentGuards(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur cpuPoint
	}{
		{"system 没涨", cpuPoint{system: 100}, cpuPoint{system: 100}},
		{"system 回退", cpuPoint{system: 200}, cpuPoint{system: 100}},
		{"total 回退", cpuPoint{total: 500, system: 100}, cpuPoint{total: 100, system: 200}},
	}
	for _, c := range cases {
		if got := cpuPercent(c.prev, c.cur, 4); got != 0 {
			t.Errorf("%s: CPU%% = %v, 期望 0", c.name, got)
		}
	}

	// 核数未知时按 1 核处理，不能除零
	if got := cpuPercent(cpuPoint{system: 0}, cpuPoint{total: 1, system: 1}, 0); got == 0 {
		t.Errorf("核数为 0 时应当兜底成 1 核")
	}
}

func TestWorkingSetCgroupV1(t *testing.T) {
	// v1: usage 10GB，其中 cache 4GB → 实际工作集 6GB
	got := workingSet(10<<30, map[string]uint64{"total_inactive_file": 4 << 30})
	if got != 6<<30 {
		t.Errorf("workingSet = %d, 期望 %d", got, uint64(6)<<30)
	}
}

func TestWorkingSetCgroupV2(t *testing.T) {
	got := workingSet(10<<30, map[string]uint64{"inactive_file": 3 << 30})
	if got != 7<<30 {
		t.Errorf("workingSet = %d, 期望 %d", got, uint64(7)<<30)
	}
}

// cache 大于 usage 是异常数据，不能减出负数。
func TestWorkingSetNoUnderflow(t *testing.T) {
	got := workingSet(100, map[string]uint64{"inactive_file": 500})
	if got != 100 {
		t.Errorf("workingSet = %d, 期望原值 100", got)
	}
}

func TestWorkingSetNoStats(t *testing.T) {
	if got := workingSet(1234, nil); got != 1234 {
		t.Errorf("workingSet = %d, 期望 1234", got)
	}
}

func TestBuild(t *testing.T) {
	raw := &logsrc.ContainerStats{}
	raw.Read = time.Now()
	raw.MemoryStats.Usage = 8 << 30
	raw.MemoryStats.Limit = 16 << 30
	raw.MemoryStats.Stats = map[string]uint64{"inactive_file": 2 << 30}
	raw.Networks = map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	}{
		"eth0": {RxBytes: 1000, TxBytes: 500},
		"eth1": {RxBytes: 2000, TxBytes: 300},
	}
	raw.BlkioStats.IoServiceBytesRecursive = []struct {
		Op    string `json:"op"`
		Value uint64 `json:"value"`
	}{
		{"Read", 4096},
		{"Write", 8192},
		{"Sync", 999}, // 不认识的 op 应当忽略
	}
	raw.PidsStats.Current = 38

	s := build("abc", raw)

	if s.MemUsed != 6<<30 {
		t.Errorf("MemUsed = %d, 期望 %d", s.MemUsed, uint64(6)<<30)
	}
	if s.MemPct < 37.4 || s.MemPct > 37.6 {
		t.Errorf("MemPct = %.2f, 期望 ~37.5", s.MemPct)
	}
	if s.NetRx != 3000 || s.NetTx != 800 {
		t.Errorf("网络流量 = rx%d tx%d, 期望 rx3000 tx800", s.NetRx, s.NetTx)
	}
	if s.BlockRead != 4096 || s.BlockWrite != 8192 {
		t.Errorf("块 IO = r%d w%d", s.BlockRead, s.BlockWrite)
	}
	if s.Pids != 38 {
		t.Errorf("Pids = %d, 期望 38", s.Pids)
	}
}

// limit 为 0 时（无限制容器）不能算出 Inf/NaN
func TestBuildZeroLimit(t *testing.T) {
	raw := &logsrc.ContainerStats{}
	raw.MemoryStats.Usage = 100 << 20

	s := build("x", raw)
	if s.MemPct != 0 {
		t.Errorf("MemPct = %v, 期望 0", s.MemPct)
	}
}

func TestSnapshotIsCopy(t *testing.T) {
	c := New(nil, func() []containers.Container { return nil }, 0, nil)
	c.snap["a"] = Sample{ID: "a", CPU: 1}

	got := c.Snapshot()
	got["a"] = Sample{ID: "a", CPU: 999}

	if c.snap["a"].CPU != 1 {
		t.Errorf("Snapshot 应当返回副本，原数据被改成了 %v", c.snap["a"].CPU)
	}
	if c.interval != 2*time.Second {
		t.Errorf("零值 interval 应被兜底成 2s, 实际 %v", c.interval)
	}
}

// fakeDaemon 顶替 docker 守护进程，按调用次数返回递增的 CPU 计数器。
//
// 每次调用 system 涨 2e9（2 核各 1 秒），total 涨 1e9 →
// 相当于占满 1 个核，按 docker 口径就是 100%。
func fakeDaemon(t *testing.T, fail bool) *httptest.Server {
	t.Helper()

	var n int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		if !strings.Contains(r.URL.Path, "/stats") {
			http.NotFound(w, r)
			return
		}

		i := int64(atomic.AddInt32(&n, 1) - 1)
		body := map[string]any{
			"read": time.Now().Format(time.RFC3339Nano),
			"cpu_stats": map[string]any{
				"cpu_usage":        map[string]any{"total_usage": i * 1_000_000_000},
				"system_cpu_usage": 100_000_000_000 + i*2_000_000_000,
				"online_cpus":      2,
			},
			"memory_stats": map[string]any{
				"usage": uint64(8) << 30,
				"limit": uint64(16) << 30,
				"stats": map[string]uint64{"inactive_file": 2 << 30},
			},
			"pids_stats": map[string]any{"current": 38},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func TestCollectorComputesCPUAcrossRounds(t *testing.T) {
	srv := fakeDaemon(t, false)
	defer srv.Close()

	api := logsrc.NewDockerAPI("tcp://" + srv.Listener.Addr().String())
	list := func() []containers.Container {
		return []containers.Container{{ID: "abc", Name: "web", Running: true}}
	}

	c := New(api, list, time.Second, nil)
	ctx := context.Background()

	// 第一轮只建立基线，还没有上一次采样可比较
	c.collect(ctx)
	first := c.Snapshot()["abc"]
	if first.CPU != 0 {
		t.Errorf("首轮 CPU%% = %v, 期望 0（还没有基线）", first.CPU)
	}
	if first.MemUsed != 6<<30 {
		t.Errorf("MemUsed = %d, 期望 %d", first.MemUsed, uint64(6)<<30)
	}

	// 第二轮才有增量：2 核里占满 1 核 = 100%（docker stats 的口径）
	c.collect(ctx)
	second := c.Snapshot()["abc"]
	if second.CPU < 99 || second.CPU > 101 {
		t.Errorf("第二轮 CPU%% = %.2f, 期望 ~100", second.CPU)
	}
	if second.MemPct < 37.4 || second.MemPct > 37.6 {
		t.Errorf("MemPct = %.2f, 期望 ~37.5", second.MemPct)
	}
	if second.Pids != 38 {
		t.Errorf("Pids = %d, 期望 38", second.Pids)
	}
	if second.At == 0 {
		t.Errorf("采样时间戳不该是 0")
	}
}

// 容器停止后应从前端能看到的快照里消失，而不是留一个永远不会更新的旧值。
func TestCollectorDropsStoppedContainers(t *testing.T) {
	srv := fakeDaemon(t, false)
	defer srv.Close()

	api := logsrc.NewDockerAPI("tcp://" + srv.Listener.Addr().String())

	running := true
	list := func() []containers.Container {
		return []containers.Container{{ID: "abc", Name: "web", Running: running}}
	}

	c := New(api, list, time.Second, nil)
	ctx := context.Background()

	c.collect(ctx)
	if _, ok := c.Snapshot()["abc"]; !ok {
		t.Fatal("首轮应当有数据")
	}

	running = false
	c.collect(ctx)
	if _, ok := c.Snapshot()["abc"]; ok {
		t.Errorf("容器停止后不该再留在快照里")
	}
}

// 守护进程报错时要把错误记在样本上，让前端知道这个数字不可信。
func TestCollectorRecordsError(t *testing.T) {
	srv := fakeDaemon(t, true)
	defer srv.Close()

	api := logsrc.NewDockerAPI("tcp://" + srv.Listener.Addr().String())
	list := func() []containers.Container {
		return []containers.Container{{ID: "abc", Name: "web", Running: true}}
	}

	c := New(api, list, time.Second, nil)
	c.collect(context.Background())

	s, ok := c.Snapshot()["abc"]
	if !ok {
		t.Fatal("失败时也应当留一条样本")
	}
	if s.Err == "" {
		t.Errorf("样本应当带上错误信息")
	}
}
