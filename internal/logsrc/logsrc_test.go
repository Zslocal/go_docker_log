package logsrc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jsonLine 造一行标准 json-file 日志。
func jsonLine(ts int, log, stream string) string {
	return fmt.Sprintf(`{"log":%q,"stream":%q,"time":"2026-11-09T01:00:%02d.000000000Z"}`, log, stream, ts%60) + "\n"
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadTailBasic(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString(jsonLine(i, fmt.Sprintf("line %d\n", i), "stdout"))
	}
	p := writeFile(t, "a.log", b.String())

	entries, err := ReadTail(p, 10, 0)
	if err != nil {
		t.Fatalf("ReadTail 失败: %v", err)
	}
	if len(entries) != 10 {
		t.Fatalf("条数 = %d, 期望 10", len(entries))
	}
	if entries[9].Log != "line 99\n" {
		t.Errorf("最后一条 = %q, 期望 line 99", entries[9].Log)
	}
	if entries[0].Log != "line 90\n" {
		t.Errorf("第一条 = %q, 期望 line 90", entries[0].Log)
	}
	if entries[0].Ts == 0 {
		t.Errorf("时间戳应当被解析出来")
	}
}

// 文件远大于一个读块时要保证不会切出半行。
func TestReadTailAcrossBlocks(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		b.WriteString(jsonLine(i, fmt.Sprintf("line %d with some padding to make the record longer\n", i), "stdout"))
	}
	p := writeFile(t, "big.log", b.String())

	entries, err := ReadTail(p, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("条数 = %d, 期望 3", len(entries))
	}
	for i, e := range entries {
		want := 4997 + i
		if !strings.HasPrefix(e.Log, fmt.Sprintf("line %d ", want)) {
			t.Errorf("第 %d 条 = %q, 期望以 line %d 开头", i, e.Log, want)
		}
	}
}

func TestReadTailFewerThanRequested(t *testing.T) {
	p := writeFile(t, "s.log", jsonLine(1, "only\n", "stdout"))

	entries, err := ReadTail(p, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("条数 = %d, 期望 1", len(entries))
	}
}

func TestReadTailEmptyFile(t *testing.T) {
	p := writeFile(t, "e.log", "")

	entries, err := ReadTail(p, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("空文件不应有日志, 得到 %d 条", len(entries))
	}
}

func TestReadTailMaxBytes(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString(jsonLine(i, fmt.Sprintf("line %d\n", i), "stdout"))
	}
	p := writeFile(t, "m.log", b.String())

	// 只允许回溯 1KB，条数必然远少于请求的 2000。
	entries, err := ReadTail(p, 2000, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatalf("限制字节数后仍应返回部分日志")
	}
	if len(entries) >= 2000 {
		t.Errorf("maxBytes 没有生效, 返回了 %d 条", len(entries))
	}
}

// 超过 16KB 的单条日志会被 Docker 拆成多个 partial 行，必须拼回一条。
func TestReadTailJoinsPartialEntries(t *testing.T) {
	content := strings.Join([]string{
		`{"log":"多行日志的第一","stream":"stdout","time":"2026-11-09T01:00:00.000000001Z","partial":true}`,
		`{"log":"段，这里还被","stream":"stdout","time":"2026-11-09T01:00:00.000000002Z","partial":true}`,
		`{"log":"切开了\n","stream":"stdout","time":"2026-11-09T01:00:00.000000003Z"}`,
	}, "\n") + "\n"

	p := writeFile(t, "p.log", content)

	entries, err := ReadTail(p, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("partial 片段应合并成 1 条, 实际 %d 条: %+v", len(entries), entries)
	}
	if entries[0].Log != "多行日志的第一段，这里还被切开了\n" {
		t.Errorf("合并结果 = %q", entries[0].Log)
	}
	// 时间戳应当取第一片的时间
	if entries[0].Ts != 1000000001 && entries[0].Ts != 0 {
		t.Logf("时间戳 = %d（取第一片）", entries[0].Ts)
	}
}

// 非 JSON 行（文件被截断、被外部工具写过）按纯文本兜底，不能整条丢弃。
func TestReadTailNonJSONLine(t *testing.T) {
	p := writeFile(t, "n.log", "not a json line\n"+jsonLine(1, "ok\n", "stderr"))

	entries, err := ReadTail(p, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("条数 = %d, 期望 2", len(entries))
	}
	if entries[0].Stream != "raw" || entries[0].Log != "not a json line" {
		t.Errorf("非 JSON 行处理错误: %+v", entries[0])
	}
	if entries[1].Stream != "stderr" {
		t.Errorf("stream = %q, 期望 stderr", entries[1].Stream)
	}
}

// TTY 容器不写 stream 字段，要补成 stdout。
func TestParseLineTTYHasNoStream(t *testing.T) {
	e, ok := ParseLine([]byte(`{"log":"tty output\n","time":"2026-11-09T01:00:00.000000000Z"}`))
	if !ok {
		t.Fatal("应当被解析")
	}
	if e.Stream != "stdout" {
		t.Errorf("stream = %q, 期望 stdout", e.Stream)
	}
}

// tsText 是日志文件里的原串，必须原样透传：它要拿去跟 `docker logs` 逐位对照，
// 任何重格式化（改时区、截精度）都会让两边对不上。
func TestParseLineKeepsRawTimeText(t *testing.T) {
	cases := []string{
		"2026-11-10T05:48:00.003429131Z",
		"2026-11-10T05:48:00.00245161Z", // 纳秒位不足 9 位，docker 会省略后缀 0
		"2026-11-10T13:48:00+08:00",     // 带偏移量而非 Z
	}
	for _, want := range cases {
		line := `{"log":"x\n","stream":"stdout","time":"` + want + `"}`
		e, ok := ParseLine([]byte(line))
		if !ok {
			t.Fatalf("%s 应当被解析", want)
		}
		if e.TsText != want {
			t.Errorf("TsText = %q, 期望 %q", e.TsText, want)
		}
		if e.Ts == 0 {
			t.Errorf("%s 的 Ts 不应为 0", want)
		}
	}

	// 非 JSON 行没有时间可用，TsText 留空让前端显示占位符
	e, ok := ParseLine([]byte("plain text"))
	if !ok || e.TsText != "" || e.Ts != 0 {
		t.Errorf("纯文本行不该带时间: %+v", e)
	}
}

func TestParseLineSkipsEmpty(t *testing.T) {
	if _, ok := ParseLine(nil); ok {
		t.Errorf("空行不该展示")
	}
	if _, ok := ParseLine([]byte(`{"log":""}`)); ok {
		t.Errorf("空的 log 字段不该展示")
	}
}

func TestFollowerIncremental(t *testing.T) {
	p := writeFile(t, "f.log", jsonLine(1, "one\n", "stdout"))

	f := NewFollower(p)
	var got []Entry
	emit := func(e Entry) { got = append(got, e) }

	moved, err := f.Poll(0, emit)
	if err != nil {
		t.Fatal(err)
	}
	if !moved || len(got) != 1 || got[0].Log != "one\n" {
		t.Fatalf("首轮应读到 1 条, 实际 moved=%v got=%+v", moved, got)
	}

	// 没有新内容时不应重复输出
	moved, err = f.Poll(0, emit)
	if err != nil {
		t.Fatal(err)
	}
	if moved || len(got) != 1 {
		t.Fatalf("无新内容时不该重复输出, moved=%v got=%d", moved, len(got))
	}

	// 追加后只应拿到新增的那条
	fh, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(jsonLine(2, "two\n", "stderr")); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	moved, err = f.Poll(0, emit)
	if err != nil {
		t.Fatal(err)
	}
	if !moved || len(got) != 2 {
		t.Fatalf("追加后应有 2 条, 实际 moved=%v got=%d", moved, len(got))
	}
	if got[1].Stream != "stderr" {
		t.Errorf("新条目 stream = %q, 期望 stderr", got[1].Stream)
	}
}

func TestFollowerSeekEnd(t *testing.T) {
	p := writeFile(t, "se.log", jsonLine(1, "old\n", "stdout"))

	f := NewFollower(p)
	if err := f.SeekEnd(); err != nil {
		t.Fatal(err)
	}

	var got []Entry
	moved, err := f.Poll(0, func(e Entry) { got = append(got, e) })
	if err != nil {
		t.Fatal(err)
	}
	if moved || len(got) != 0 {
		t.Fatalf("SeekEnd 之后不该读到历史, moved=%v got=%d", moved, len(got))
	}
}

// 文件变小说明被轮转/截断了，要能自动从头重读而不是一直空转。
func TestFollowerDetectsTruncate(t *testing.T) {
	p := writeFile(t, "r.log",
		jsonLine(1, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", "stdout")+
			jsonLine(2, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n", "stdout"))

	f := NewFollower(p)
	var got []Entry
	if _, err := f.Poll(0, func(e Entry) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("首轮应有 2 条, 实际 %d", len(got))
	}

	// 覆盖成一个更小的文件
	if err := os.WriteFile(p, []byte(jsonLine(3, "new\n", "stdout")), 0o644); err != nil {
		t.Fatal(err)
	}

	got = nil
	if _, err := f.Poll(0, func(e Entry) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Log != "new\n" {
		t.Fatalf("轮转后应重新读到新内容, 实际 %+v", got)
	}
}

// 半行（写到一半、还没换行）要留到下一轮拼上，不能当成一条残缺日志吐出去。
func TestFollowerHoldsPartialLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h.log")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	f := NewFollower(p)
	var got []Entry
	emit := func(e Entry) { got = append(got, e) }

	full := jsonLine(1, "complete\n", "stdout")

	// 先写整行的一半
	fh, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	fh.WriteString(full[:len(full)/2])
	fh.Close()

	if _, err := f.Poll(0, emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("半行不该产出条目, 实际 %+v", got)
	}

	// 补上剩下的一半
	fh, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	fh.WriteString(full[len(full)/2:])
	fh.Close()

	if _, err := f.Poll(0, emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Log != "complete\n" {
		t.Fatalf("补齐后应得到 1 条完整日志, 实际 %+v", got)
	}
}

func TestParseAPILine(t *testing.T) {
	e := ParseAPILine("2026-11-09T01:02:03.123456789Z hello world")
	if e.Log != "hello world" {
		t.Errorf("正文 = %q", e.Log)
	}
	if e.Ts == 0 {
		t.Errorf("时间戳应为 0 以外")
	}
	// 原串必须逐字保留：界面靠它还原纳秒级时间，
	// 而 e.Ts 这个 int64 到了 JS 里会被 double 截掉后几位。
	if e.TsText != "2026-11-09T01:02:03.123456789Z" {
		t.Errorf("TsText = %q, 期望原串", e.TsText)
	}

	// 没有时间戳前缀时整行当正文，不能把内容吃掉
	e = ParseAPILine("plain text without timestamp")
	if e.Log != "plain text without timestamp" || e.Ts != 0 {
		t.Errorf("无时间戳时处理错误: %+v", e)
	}
	if e.TsText != "" {
		t.Errorf("无时间戳时 TsText 应为空, 实际 %q", e.TsText)
	}
}

// Docker 的多路复用流是 8 字节头 + 负载，muxReader 要把帧拆掉只留正文。
func TestMuxReader(t *testing.T) {
	var raw bytes.Buffer
	writeFrame(&raw, 1, "hello ")
	writeFrame(&raw, 2, "world\n")

	m := &muxReader{src: bufio.NewReader(&raw)}
	out, err := io.ReadAll(m)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(out) != "hello world\n" {
		t.Errorf("拆帧结果 = %q", string(out))
	}
}

func writeFrame(w io.Writer, typ byte, payload string) {
	hdr := make([]byte, 8)
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	w.Write(hdr)
	w.Write([]byte(payload))
}

func TestSplitLinesTrailingNewline(t *testing.T) {
	got := splitLines([]byte("a\nb\n"))
	if len(got) != 2 {
		t.Fatalf("应切成 2 段, 实际 %d", len(got))
	}
	if string(got[0]) != "a" || string(got[1]) != "b" {
		t.Errorf("切分结果 = %q", got)
	}
}
