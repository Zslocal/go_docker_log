// Package logsrc 负责把 Docker 的日志文件读成结构化条目。
//
// Docker 的 json-file（以及 local）驱动写的是 NDJSON：一行一个 JSON 对象
//
//	{"log":"hello\n","stream":"stdout","time":"2026-11-09T01:23:45.123456789Z"}
//
// 有两个坑必须处理：
//   - TTY 容器（docker run -t）没有 "stream" 字段，需要自己补成 stdout；
//   - 单条日志超过 16KB 时会被拆成多行，前几行带 "partial":true，最后一行不带。
//     直接按行展示会把一条日志切成好几段，必须拼回去。
package logsrc

import (
	"bytes"
	"encoding/json"
	"time"
)

// MaxScanToken 是 bufio.Scanner 的单行上限，供上层读 docker API 流时使用。
// 取 4MB，与文件模式的 maxLineBytes 保持一致：一条日志超过 4MB 已经很反常，
// 但仍要保证不会因为 Scanner 默认的 64KB 限制而整段丢弃。
const MaxScanToken = 4 << 20

// Entry 是归一化后的一条日志，直接用于 JSON 输出和 SSE 推送。
type Entry struct {
	// Ts 是 Unix 纳秒时间戳，0 表示该行没有可用时间（例如文件被外部工具截断）。
	Ts int64 `json:"ts"`
	// TsText 是 docker 写进日志文件里的原始时间串（RFC3339Nano，UTC），
	// 只做透传不改写。界面要靠它显示得跟 `docker logs` 里一模一样：
	// 纳秒时间戳约 1.7e18，超过 JS 的 Number.MAX_SAFE_INTEGER(9e15)，
	// 转成数字再格式化必然丢精度，只有原串能保住纳秒位。
	TsText string `json:"tsText"`
	// Stream 取 stdout / stderr / raw（raw 表示这一行不是合法 JSON，按纯文本兜底）。
	Stream string `json:"stream"`
	// Log 是日志正文，保留原始换行。
	Log string `json:"log"`
}

// rawEntry 对应 Docker 写的单行 JSON。
type rawEntry struct {
	Log     string `json:"log"`
	Stream  string `json:"stream"`
	Time    string `json:"time"`
	Partial bool   `json:"partial"`
}

// Time 解析该行时间戳，格式固定为 RFC3339Nano。
func (r rawEntry) parseTime() int64 {
	if r.Time == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, r.Time)
	if err != nil {
		return 0
	}
	return t.UnixNano()
}

// ParseLine 解析一行日志。第二个返回值表示这一行是否值得展示（空行会被丢弃）。
//
// 非 JSON 行不当作错误：日志文件可能被 logrotate、被 `> file` 截断、
// 或者用户手改过，此时按纯文本显示比整条丢弃有用得多。
func ParseLine(line []byte) (Entry, bool) {
	line = bytes.TrimRight(line, "\r")
	if len(line) == 0 {
		return Entry{}, false
	}

	var raw rawEntry
	if err := json.Unmarshal(line, &raw); err != nil {
		return Entry{Stream: "raw", Log: string(line)}, true
	}

	stream := raw.Stream
	if stream == "" {
		// TTY 容器的多路复用被关掉了，Docker 不写 stream 字段。
		stream = "stdout"
	}
	if raw.Log == "" && raw.Time == "" {
		// {"log":""} 这种空心跳，没有展示价值。
		return Entry{}, false
	}

	return Entry{Ts: raw.parseTime(), TsText: raw.Time, Stream: stream, Log: raw.Log}, true
}

// assembler 把被拆开的多行日志拼回完整一条。
//
// Docker 的切分规则是：一条超长日志被拆成 N 个 JSON 行，前 N-1 个标 partial:true，
// 最后一个不标。所以只要见到 partial 就一直往 pending 里追加，遇到不带的就收尾输出。
type assembler struct {
	pending *Entry
}

// Feed 喂入一行原始记录，凑成完整日志后回调 emit。
func (a *assembler) Feed(raw rawEntry, emit func(Entry)) {
	if raw.Log == "" && raw.Time == "" {
		return
	}

	stream := raw.Stream
	if stream == "" {
		stream = "stdout"
	}
	// 时间戳取第一片的：那才是这条日志真正被写下的时刻。
	e := Entry{Ts: raw.parseTime(), TsText: raw.Time, Stream: stream, Log: raw.Log}

	switch {
	case raw.Partial:
		// 片段还在继续，先攒着；时间戳保留第一片的，才是这条日志真实的产生时间。
		if a.pending == nil {
			a.pending = &e
		} else {
			a.pending.Log += e.Log
		}
	case a.pending != nil:
		// 收尾片段，拼上之后整条输出。
		a.pending.Log += e.Log
		emit(*a.pending)
		a.pending = nil
	default:
		emit(e)
	}
}

// Flush 输出残留的片段。文件末尾刚好停在 partial 中间时用得上，
// 宁可显示一条被截断的日志，也不要把它吞掉。
func (a *assembler) Flush(emit func(Entry)) {
	if a.pending != nil {
		emit(*a.pending)
		a.pending = nil
	}
}

// reset 清空状态，日志轮转时调用。
func (a *assembler) reset() {
	a.pending = nil
}
