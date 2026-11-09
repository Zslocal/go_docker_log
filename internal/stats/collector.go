// Package stats 周期性地从 docker 守护进程采集容器资源占用，缓存成一份快照。
//
// 为什么放在后台采而不是每次请求现取：
//   - docker stats 每个容器要单独一次请求，18 个容器就是 18 次往返；
//   - CPU 百分比本质上是两次采样之间的增量，必须有上一次的数据才能算；
//   - 前端只关心"现在的值"，读缓存即可，延迟 2 秒对看板完全够用。
//
// 数值口径对齐 docker stats 命令，避免同一个指标在两个地方显示两个数。
package stats

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/Zslocal/go_docker_log/internal/containers"
	"github.com/Zslocal/go_docker_log/internal/logsrc"
)

// Sample 是单个容器的一次资源快照。
type Sample struct {
	ID string `json:"id"`

	// CPU 是 CPU 占用百分比，口径同 docker stats：
	// 单核跑满 = 100%，多核可以超过 100%（4 核跑满 = 400%）。
	CPU float64 `json:"cpu"`

	MemUsed  uint64  `json:"memUsed"`
	MemLimit uint64  `json:"memLimit"`
	MemPct   float64 `json:"memPct"`

	NetRx uint64 `json:"netRx"`
	NetTx uint64 `json:"netTx"`

	BlockRead  uint64 `json:"blockRead"`
	BlockWrite uint64 `json:"blockWrite"`

	Pids uint64 `json:"pids"`

	// At 是采样时刻（Unix 毫秒），前端可据此判断数据是否新鲜。
	At int64 `json:"at"`
	// Err 非空表示这一轮采集失败，其余字段不可信。
	Err string `json:"err,omitempty"`
}

// cpuPoint 是一次 CPU 采样的两个计数器，两次相减得到增量。
type cpuPoint struct {
	total  uint64 // 容器累计 CPU 时间
	system uint64 // 宿主机累计 CPU 时间
}

// Collector 是后台采集器。
type Collector struct {
	api      *logsrc.DockerAPI
	list     func() []containers.Container
	interval time.Duration
	logger   *log.Logger

	mu   sync.RWMutex
	snap map[string]Sample
	prev map[string]cpuPoint

	// failStreak 记录连续失败次数，只在状态切换时打日志，避免刷屏。
	failStreak int
}

// New 构造采集器。list 每次返回当前容器列表，采集器只处理其中 running 的。
func New(api *logsrc.DockerAPI, list func() []containers.Container, interval time.Duration, logger *log.Logger) *Collector {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &Collector{
		api:      api,
		list:     list,
		interval: interval,
		logger:   logger,
		snap:     make(map[string]Sample),
		prev:     make(map[string]cpuPoint),
	}
}

// Run 阻塞运行采集循环，直到 ctx 取消。
func (c *Collector) Run(ctx context.Context) {
	// 先立刻采一次建立基线：CPU 需要两次采样才能算出增量，
	// 提前一轮采完，用户打开页面时第一个数字就是准的。
	c.collect(ctx)

	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.collect(ctx)
		}
	}
}

// Snapshot 返回当前快照的副本。
func (c *Collector) Snapshot() map[string]Sample {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make(map[string]Sample, len(c.snap))
	for k, v := range c.snap {
		out[k] = v
	}
	return out
}

// collect 对一轮所有运行中的容器并发采样。
func (c *Collector) collect(ctx context.Context) {
	all := c.list()
	running := make([]containers.Container, 0, len(all))
	// alive 只收运行中的容器：停止的容器如果没有被清理，前端会一直显示
	// 它退出前的最后一个数字，看起来像"还活着"。
	alive := make(map[string]struct{}, len(all))

	for _, ct := range all {
		if !ct.Running {
			continue
		}
		alive[ct.ID] = struct{}{}
		running = append(running, ct)
	}

	// 已经消失或已停止的容器不用再留着占地方。
	c.mu.Lock()
	for id := range c.snap {
		if _, ok := alive[id]; !ok {
			delete(c.snap, id)
		}
	}
	for id := range c.prev {
		if _, ok := alive[id]; !ok {
			delete(c.prev, id)
		}
	}
	c.mu.Unlock()

	if len(running) == 0 {
		return
	}

	// 限制并发，十几二十个容器同时打 socket 没必要。
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup

	for _, ct := range running {
		wg.Add(1)
		go func(ct containers.Container) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			c.fetchOne(ctx, ct)
		}(ct)
	}
	wg.Wait()
}

// fetchOne 采一个容器。
func (c *Collector) fetchOne(ctx context.Context, ct containers.Container) {
	// 单个容器的请求超时单独控制：某个容器卡住不能拖垮整轮。
	sctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	raw, err := c.api.Stats(sctx, ct.ID)
	if err != nil {
		c.mu.Lock()
		c.snap[ct.ID] = Sample{ID: ct.ID, At: time.Now().UnixMilli(), Err: err.Error()}
		c.mu.Unlock()
		c.noteFailure(ct.Name, err)
		return
	}
	c.noteSuccess()

	s := build(ct.ID, raw)

	c.mu.Lock()
	prev, hasPrev := c.prev[ct.ID]
	c.mu.Unlock()

	if hasPrev {
		cpus := float64(raw.CPUStats.OnlineCPUs)
		if cpus == 0 {
			cpus = float64(len(raw.CPUStats.CPUUsage.PercpuUsage))
		}
		s.CPU = cpuPercent(prev, cpuPoint{
			total:  raw.CPUStats.CPUUsage.TotalUsage,
			system: raw.CPUStats.SystemUsage,
		}, cpus)
	}

	c.mu.Lock()
	c.prev[ct.ID] = cpuPoint{
		total:  raw.CPUStats.CPUUsage.TotalUsage,
		system: raw.CPUStats.SystemUsage,
	}
	c.snap[ct.ID] = s
	c.mu.Unlock()
}

// noteFailure 只在"从正常转为持续失败"时打一次日志。
func (c *Collector) noteFailure(name string, err error) {
	c.mu.Lock()
	c.failStreak++
	first := c.failStreak == 1
	c.mu.Unlock()

	if first && c.logger != nil {
		c.logger.Printf("采集容器 %s 资源占用失败（后续同类错误不再重复打印）: %v", name, err)
	}
}

func (c *Collector) noteSuccess() {
	c.mu.Lock()
	c.failStreak = 0
	c.mu.Unlock()
}

// build 把 docker 的原始响应换算成 Sample（不含 CPU，CPU 需要前后两次采样）。
func build(id string, raw *logsrc.ContainerStats) Sample {
	s := Sample{
		ID:       id,
		MemLimit: raw.MemoryStats.Limit,
		Pids:     raw.PidsStats.Current,
		At:       time.Now().UnixMilli(),
	}

	s.MemUsed = workingSet(raw.MemoryStats.Usage, raw.MemoryStats.Stats)
	if s.MemLimit > 0 {
		s.MemPct = float64(s.MemUsed) / float64(s.MemLimit) * 100
	}

	for _, n := range raw.Networks {
		s.NetRx += n.RxBytes
		s.NetTx += n.TxBytes
	}

	for _, b := range raw.BlkioStats.IoServiceBytesRecursive {
		switch b.Op {
		case "Read", "read":
			s.BlockRead += b.Value
		case "Write", "write":
			s.BlockWrite += b.Value
		}
	}

	// 极端情况下（容器刚创建）可能还没有时间戳，用当前时间兜底。
	if !raw.Read.IsZero() {
		s.At = raw.Read.UnixMilli()
	}
	return s
}

// cpuPercent 按 docker 官方算法计算两次采样之间的 CPU 百分比：
//
//	(容器 CPU 增量 / 宿主机 CPU 增量) * 核数 * 100
//
// 分母用宿主机的总 CPU 时间增量，所以结果天然是"占整机的比例 × 核数"，
// 与 docker stats 的显示一致。
func cpuPercent(prev, cur cpuPoint, cpus float64) float64 {
	if cur.system <= prev.system || cur.total < prev.total {
		return 0
	}
	if cpus <= 0 {
		cpus = 1
	}
	delta := float64(cur.total - prev.total)
	systemDelta := float64(cur.system - prev.system)
	return delta / systemDelta * cpus * 100
}

// workingSet 复现 docker stats 的 MEM USAGE 口径：从 usage 里扣掉 page cache。
//
// cgroup v1 的字段叫 total_inactive_file，v2 叫 inactive_file，两个都要认。
// 不扣的话，只要容器读过一批文件，"内存占用"就会虚高到接近 limit。
func workingSet(usage uint64, st map[string]uint64) uint64 {
	for _, key := range []string{"total_inactive_file", "inactive_file"} {
		if v, ok := st[key]; ok && v > 0 && v < usage {
			return usage - v
		}
	}
	return usage
}
