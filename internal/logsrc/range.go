package logsrc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
)

const (
	// RangeMaxEntries 是一次时间段查询最多回传的条数。前端是逐行建 DOM 的，
	// 再多浏览器就顶不住了；超出时结果里会标 Truncated。
	RangeMaxEntries = 20000
	// RangeMaxBytes 限制一次查询向后扫多少字节，
	// 防止有人选了个横跨几个 GB 的窗口把 IO 打满。
	RangeMaxBytes int64 = 256 << 20
	// probeLine 是二分定位时一次读的字节数，够覆盖绝大多数单行日志。
	probeLine = 64 << 10
	// seekChunk 是找换行符时的读块大小。
	seekChunk = 32 << 10
)

// Range 描述一次按时间段取日志的条件。From/To 为 Unix 纳秒，0 表示该侧不限。
//
// 关键点：Docker 的 json-file 是严格按时间追加写的，所以文件偏移和日志时间
// 是单调对应的。有了这个性质就不必从头扫——直接二分跳到 From 对应的字节位置，
// 时间段查询的代价只跟窗口内的数据量有关，跟文件多大无关。
type Range struct {
	From     int64
	To       int64
	Limit    int
	MaxBytes int64
}

// RangeResult 是一次时间段查询的结果。
type RangeResult struct {
	Entries []Entry
	// Total 是窗口内实际有多少条，可能大于 len(Entries)。
	Total int
	// Truncated 表示条数超过 Limit，只回传了前 Limit 条。
	Truncated bool
	// Cut 表示窗口超出 MaxBytes，只读到了中间某一处。
	Cut bool
	// FirstAt / LastAt 是整个文件的时间跨度（原始时间串），
	// 查不到东西时用它告诉用户"日志里其实只有哪一段"。
	FirstAt string
	LastAt  string
	// Note 用于解释结果为什么可能不完整（例如日志不在本地文件里）。
	Note  string
	Bytes int64
}

// ReadRange 取出落在 [From, To] 区间内的日志。
//
// 返回的条目按时间升序，与文件顺序一致。
func ReadRange(filename string, opt Range) (*RangeResult, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}

	if opt.Limit <= 0 || opt.Limit > RangeMaxEntries {
		opt.Limit = RangeMaxEntries
	}
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = RangeMaxBytes
	}

	res := &RangeResult{}
	if size == 0 {
		return res, nil
	}

	res.FirstAt, res.LastAt = fileSpan(f, size)

	start := int64(0)
	if opt.From > 0 {
		if start, err = seekTime(f, size, opt.From); err != nil {
			return nil, err
		}
	}

	// 只给上界（"X 点之前"）时没有便宜的定位办法，从头顺着读，
	// 读到第一条晚于 To 的日志就停；MaxBytes 兜住最坏情况。
	end := size
	if end-start > opt.MaxBytes {
		end = start + opt.MaxBytes
		res.Cut = true
	}
	if start >= end {
		return res, nil
	}
	res.Bytes = end - start

	sr := io.NewSectionReader(f, start, end-start)
	br := bufio.NewReaderSize(sr, 256<<10)

	var asm assembler
	stop := false
	emit := func(e Entry) {
		if stop {
			return
		}
		// 二分定位只是加速手段，这里再按时间戳兜一层：
		// 起点万一落在窗口前一点点，也不会把窗口外的日志混进来。
		if opt.From > 0 && e.Ts > 0 && e.Ts < opt.From {
			return
		}
		res.Total++
		if len(res.Entries) < opt.Limit {
			res.Entries = append(res.Entries, e)
		} else {
			res.Truncated = true
		}
	}

	for !stop {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			if len(line) > 0 {
				var r rawEntry
				if json.Unmarshal(line, &r) != nil {
					emit(Entry{Stream: "raw", Log: string(line)})
				} else {
					if opt.To > 0 && r.Time != "" {
						if ts := r.parseTime(); ts > opt.To {
							stop = true
						}
					}
					if !stop {
						asm.Feed(r, emit)
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	asm.Flush(emit)

	return res, nil
}

// SpanFile 只读首行和尾块，回答"这个文件覆盖哪段时间"。
// 用户设时间段之前先看一眼，才知道自己要查的那段到底在不在里面。
func SpanFile(filename string) (first, last string, size int64, err error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", "", 0, err
	}
	defer f.Close()

	size, err = f.Seek(0, io.SeekEnd)
	if err != nil {
		return "", "", 0, err
	}
	if size == 0 {
		return "", "", 0, nil
	}
	first, last = fileSpan(f, size)
	return first, last, size, nil
}

// ---------------------------------------------------------------- 二分定位

// seekTime 返回第一条时间戳 >= target 的行的起始偏移；都比 target 早就返回 size。
//
// 不变式：lo 永远是某一行的行首，hi 要么是 size 要么也是行首。
// 每次迭代都把区间收窄到行边界上，所以最终 lo 可以直接 Seek 过去按行读。
// 探测点恰好落在行边界上时可能收不动区间，这种情况直接返回 lo——宁可多读
// 几行（ReadRange 会按时间戳再筛一遍），也不能漏掉窗口开头的日志。
func seekTime(f *os.File, size, target int64) (int64, error) {
	if size <= 0 {
		return 0, nil
	}

	lo, hi := int64(0), size
	for iter := 0; lo < hi && iter < 64; iter++ {
		mid := lo + (hi-lo)/2
		li, ok := lineAt(f, mid, size)
		if !ok {
			// mid 落在末尾那段没有换行的残行里，往回退。
			hi = mid
			continue
		}
		if li.ts >= target {
			if li.pos <= lo || li.pos >= hi {
				return lo, nil
			}
			hi = li.pos
			continue
		}
		if li.end <= lo {
			return lo, nil
		}
		lo = li.end
	}
	return lo, nil
}

// lineInfo 描述一行的时间信息。
type lineInfo struct {
	pos int64  // 行首偏移
	end int64  // 下一个 \n 之后的偏移
	at  string // 原始时间串
	ts  int64  // Unix 纳秒
}

// lineAt 找到 off 之后的第一整行（off==0 时就是第一行），解析它的时间。
func lineAt(f *os.File, off, size int64) (lineInfo, bool) {
	if off >= size {
		return lineInfo{}, false
	}

	pos := off
	if pos > 0 {
		nl, ok := findNewline(f, pos, size)
		if !ok {
			return lineInfo{}, false
		}
		pos = nl + 1
		if pos >= size {
			return lineInfo{}, false
		}
	}

	n := int64(probeLine)
	if size-pos < n {
		n = size - pos
	}
	buf := make([]byte, n)
	m, _ := f.ReadAt(buf, pos)
	buf = buf[:m]

	if i := bytes.IndexByte(buf, '\n'); i >= 0 {
		line := bytes.TrimRight(buf[:i], "\r")
		var r rawEntry
		if json.Unmarshal(line, &r) == nil {
			return lineInfo{pos: pos, end: pos + int64(i) + 1, at: r.Time, ts: r.parseTime()}, true
		}
		return lineInfo{pos: pos, end: pos + int64(i) + 1}, true
	}

	// 单行比 probeLine 还长（partial 片段），只为定位行尾，时间当作未知。
	if nl, ok := findNewline(f, pos, size); ok {
		return lineInfo{pos: pos, end: nl + 1}, true
	}
	return lineInfo{}, false
}

// findNewline 从 off 开始向后找第一个 \n，返回它的偏移。
func findNewline(f *os.File, off, size int64) (int64, bool) {
	buf := make([]byte, seekChunk)
	for off < size {
		n := int64(len(buf))
		if size-off < n {
			n = size - off
		}
		m, _ := f.ReadAt(buf[:n], off)
		if m == 0 {
			return 0, false
		}
		if i := bytes.IndexByte(buf[:m], '\n'); i >= 0 {
			return off + int64(i), true
		}
		off += int64(m)
	}
	return 0, false
}

// fileSpan 取第一条和最后一条日志的原始时间串。尾块可能从半行开始，
// 解析失败就继续往前翻，最多翻几块。
func fileSpan(f *os.File, size int64) (string, string) {
	var first, last string

	if li, ok := lineAt(f, 0, size); ok && li.at != "" {
		first = li.at
	}

	for off := size; off > 0 && last == ""; {
		n := int64(readBlock)
		if off < n {
			n = off
		}
		off -= n

		buf := make([]byte, n)
		m, _ := f.ReadAt(buf, off)
		if m == 0 {
			break
		}
		lines := bytes.Split(buf[:m], []byte{'\n'})
		for i := len(lines) - 1; i >= 0; i-- {
			var r rawEntry
			if json.Unmarshal(bytes.TrimRight(lines[i], "\r"), &r) == nil && r.Time != "" {
				last = r.Time
				break
			}
		}
	}

	return first, last
}
