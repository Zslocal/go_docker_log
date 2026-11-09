package logsrc

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

const (
	// readBlock 是反向扫描时的块大小，32KB 大约能装几十行普通日志。
	readBlock = 32 * 1024
	// DefaultTailBytes 限制一次最多回溯多少字节。
	// 生产容器动辄几百 MB 日志，无上限地找"最后 500 行"会把内存打爆。
	DefaultTailBytes int64 = 16 << 20
	// maxLineBytes 是单行硬上限。超过就强制切分，
	// 防止没有换行符的畸形文件把 remainder 撑成无底洞。
	maxLineBytes = 4 << 20
)

// ReadTail 读取文件末尾最多 n 条日志，最多回溯 maxBytes 字节（<=0 用默认值）。
//
// 实现上没有把整个文件读进内存：从尾部按块反向扫描，凑够 n 个换行就停。
// 这决定了它在几百 MB 的日志文件上也能毫秒级返回。
func ReadTail(filename string, n int, maxBytes int64) ([]Entry, error) {
	if n <= 0 {
		return nil, nil
	}
	if maxBytes <= 0 {
		maxBytes = DefaultTailBytes
	}

	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}

	// 反向逐块扫描，收集到切片里（顺序是倒的，最后再翻过来）。
	start := size
	var chunks [][]byte
	lines := 0
	var scanned int64
	for start > 0 && lines <= n && scanned < maxBytes {
		step := int64(readBlock)
		if step > start {
			step = start
		}
		start -= step

		chunk := make([]byte, step)
		if _, err := f.ReadAt(chunk, start); err != nil && err != io.EOF {
			return nil, err
		}
		chunks = append(chunks, chunk)
		lines += bytes.Count(chunk, []byte{'\n'})
		scanned += step
	}

	var data []byte
	for i := len(chunks) - 1; i >= 0; i-- {
		data = append(data, chunks[i]...)
	}

	// start > 0 说明没扫到文件头，第一段是从半行开始读的，必须丢掉，
	// 否则会出现一条只有后半截的假日志。
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}

	raw := splitLines(data)
	if len(raw) > n {
		raw = raw[len(raw)-n:]
	}

	out := make([]Entry, 0, len(raw))
	emit := func(e Entry) { out = append(out, e) }

	var asm assembler
	for _, ln := range raw {
		var r rawEntry
		if err := json.Unmarshal(ln, &r); err != nil {
			out = append(out, Entry{Stream: "raw", Log: string(ln)})
			continue
		}
		asm.Feed(r, emit)
	}
	asm.Flush(emit)

	// partial 拼接会让条数少于原始行数，这里再裁一次保证不超过 n。
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// splitLines 按 \n 切分，并去掉末尾因文件以换行结尾产生的空元素。
func splitLines(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte{'\n'})
	if n := len(parts); n > 0 && len(parts[n-1]) == 0 {
		parts = parts[:n-1]
	}
	return parts
}
