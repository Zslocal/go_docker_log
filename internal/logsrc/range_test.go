package logsrc

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// tsLine 造一行指定时刻的日志（秒级，纳秒补零，方便断言）。
func tsLine(t time.Time, log string) string {
	return fmt.Sprintf(`{"log":%q,"stream":"stdout","time":%q}`+"\n", log, t.UTC().Format(time.RFC3339Nano))
}

// rangeFile 造一个每秒一条、共 n 条的日志文件，返回路径和起始时间。
func rangeFile(t *testing.T, n int) (string, time.Time) {
	t.Helper()
	base := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(tsLine(base.Add(time.Duration(i)*time.Second), fmt.Sprintf("line %d\n", i)))
	}
	return writeFile(t, "range.log", b.String()), base
}

func ns(t time.Time) int64 { return t.UnixNano() }

func TestReadRangeWindow(t *testing.T) {
	p, base := rangeFile(t, 100)

	// 取第 10~14 秒这 5 条
	res, err := ReadRange(p, Range{
		From: ns(base.Add(10 * time.Second)),
		To:   ns(base.Add(14 * time.Second)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 5 {
		t.Fatalf("Total = %d, 期望 5", res.Total)
	}
	if len(res.Entries) != 5 {
		t.Fatalf("条数 = %d, 期望 5", len(res.Entries))
	}
	if res.Entries[0].Log != "line 10\n" || res.Entries[4].Log != "line 14\n" {
		t.Errorf("窗口错位: 首 %q 末 %q", res.Entries[0].Log, res.Entries[4].Log)
	}
	if res.Truncated || res.Cut {
		t.Errorf("不该有截断标记: %+v", res)
	}
}

// 时间段查询必须跟文件大小无关：二分定位不能把起点对到半行上。
func TestReadRangeBigFileAlignsToLine(t *testing.T) {
	base := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString(tsLine(base.Add(time.Duration(i)*time.Millisecond),
			fmt.Sprintf("line %d padding padding padding padding\n", i)))
	}
	p := writeFile(t, "big-range.log", b.String())

	from := base.Add(15000 * time.Millisecond)
	res, err := ReadRange(p, Range{From: ns(from)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 5000 {
		t.Fatalf("条数 = %d, 期望 5000", len(res.Entries))
	}
	if res.Entries[0].Log != "line 15000 padding padding padding padding\n" {
		t.Errorf("首条 = %q, 期望 line 15000", res.Entries[0].Log)
	}
}

func TestReadRangeOnlyFrom(t *testing.T) {
	p, base := rangeFile(t, 50)

	res, err := ReadRange(p, Range{From: ns(base.Add(45 * time.Second))})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 5 {
		t.Fatalf("Total = %d, 期望 5", res.Total)
	}
	if res.LastAt == "" || res.FirstAt == "" {
		t.Errorf("应当返回文件时间跨度, got %q ~ %q", res.FirstAt, res.LastAt)
	}
}

func TestReadRangeOnlyTo(t *testing.T) {
	p, base := rangeFile(t, 30)

	res, err := ReadRange(p, Range{To: ns(base.Add(4 * time.Second))})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 5 {
		t.Fatalf("Total = %d, 期望 5", res.Total)
	}
	if res.Entries[0].Log != "line 0\n" {
		t.Errorf("首条 = %q", res.Entries[0].Log)
	}
}

func TestReadRangeEmptyWindow(t *testing.T) {
	p, base := rangeFile(t, 10)

	res, err := ReadRange(p, Range{
		From: ns(base.Add(time.Hour)),
		To:   ns(base.Add(2 * time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 0 || len(res.Entries) != 0 {
		t.Fatalf("应当是空结果, got %d", res.Total)
	}
	// 空结果也要能告诉用户日志里到底有哪一段
	if res.FirstAt == "" || res.LastAt == "" {
		t.Errorf("空结果也要带上文件时间跨度")
	}
}

func TestReadRangeTruncated(t *testing.T) {
	p, base := rangeFile(t, 100)

	res, err := ReadRange(p, Range{From: ns(base), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Errorf("超过 Limit 时应当标记 Truncated")
	}
	if len(res.Entries) != 10 {
		t.Fatalf("条数 = %d, 期望 10", len(res.Entries))
	}
	if res.Total != 100 {
		t.Errorf("Total 应当统计窗口内全部条数, got %d", res.Total)
	}
}

func TestReadRangeByteCut(t *testing.T) {
	p, base := rangeFile(t, 1000)

	res, err := ReadRange(p, Range{From: ns(base), MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Cut {
		t.Errorf("超出 MaxBytes 时应当标记 Cut")
	}
	if res.Total == 0 || res.Total >= 1000 {
		t.Errorf("Total = %d, 应当只读到一部分", res.Total)
	}
}

// 时间窗口跨过 16KB 的 partial 片段时，拼接逻辑不能把一条日志劈开。
func TestReadRangePartial(t *testing.T) {
	p := writeFile(t, "partial.log", strings.Join([]string{
		`{"log":"head ","stream":"stdout","time":"2026-11-10T02:00:00.000000000Z","partial":true}`,
		`{"log":"tail\n","stream":"stdout","time":"2026-11-10T02:00:00.000000001Z"}`,
		`{"log":"next\n","stream":"stdout","time":"2026-11-10T02:00:01.000000000Z"}`,
	}, "\n")+"\n")

	from := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	res, err := ReadRange(p, Range{From: ns(from)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("条数 = %d, 期望 2（partial 应拼成一条）", len(res.Entries))
	}
	if res.Entries[0].Log != "head tail\n" {
		t.Errorf("拼接结果 = %q", res.Entries[0].Log)
	}
}

func TestSpanFile(t *testing.T) {
	p, base := rangeFile(t, 60)

	first, last, size, err := SpanFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 {
		t.Fatalf("size = %d", size)
	}
	wantFirst := base.UTC().Format(time.RFC3339Nano)
	wantLast := base.Add(59 * time.Second).UTC().Format(time.RFC3339Nano)
	if first != wantFirst {
		t.Errorf("first = %q, 期望 %q", first, wantFirst)
	}
	if last != wantLast {
		t.Errorf("last = %q, 期望 %q", last, wantLast)
	}
}

func TestSeekTimePastEnd(t *testing.T) {
	p, base := rangeFile(t, 5)

	res, err := ReadRange(p, Range{From: ns(base.Add(time.Hour)), To: ns(base.Add(2 * time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 0 {
		t.Fatalf("条数 = %d, 期望 0", len(res.Entries))
	}
}
