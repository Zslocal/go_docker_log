package logsrc

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// Follower 增量跟踪一个日志文件，供 SSE 实时推送使用。
//
// 没有用 fsnotify：Docker 的日志是持续小批量追加，inotify 会疯狂触发事件，
// 反而需要自己做节流；固定间隔 stat + 增量读更简单，200~300ms 的延迟
// 对看日志来说完全够用，也不需要引入第三方依赖。
//
// 每次 Poll 都重新 open 文件而不是长期持有句柄，这样日志轮转后被 rename 的
// 旧句柄不会一直读到 EOF 而不自知。
type Follower struct {
	path      string
	offset    int64
	remainder []byte
	asm       assembler
}

// NewFollower 创建跟踪器，起始位置是文件开头；要跟着"新日志"走请再调 SeekEnd。
func NewFollower(path string) *Follower {
	return &Follower{path: path}
}

// Path 返回被跟踪的文件路径。
func (f *Follower) Path() string { return f.path }

// Offset 返回当前读到的字节位置，便于调试和 Last-Event-ID 场景。
func (f *Follower) Offset() int64 { return f.offset }

// SeekEnd 把起点移到文件末尾，即只关心之后新产生的日志。
func (f *Follower) SeekEnd() error {
	st, err := os.Stat(f.path)
	if err != nil {
		return err
	}
	f.offset = st.Size()
	f.remainder = nil
	f.asm.reset()
	return nil
}

// SetOffset 手动指定起点，配合 ReadTail 可做到"先给历史、再从历史之后接着推"。
func (f *Follower) SetOffset(n int64) {
	if n < 0 {
		n = 0
	}
	f.offset = n
}

// Poll 读取自上次调用以来新增的内容，凑成完整日志后回调 emit。
// 返回值表示这一轮是否读到了字节（调用方据此决定要不要 flush SSE）。
//
// maxRead 限制单轮最多读多少字节，避免容器突然刷屏时一次性吃下几十 MB；
// 读不完的部分下一轮继续，不会丢。
func (f *Follower) Poll(maxRead int64, emit func(Entry)) (bool, error) {
	st, err := os.Stat(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			// 容器被删或正在重建，重置状态等文件回来。
			f.offset = 0
			f.remainder = nil
			f.asm.reset()
			return false, nil
		}
		return false, err
	}

	size := st.Size()

	// 文件变小 = 被轮转（新文件从 0 开始）或被截断，只能从头再读。
	// 轮转丢掉的旧内容是有意的取舍：Docker 自己也有同样的窗口。
	if size < f.offset {
		f.offset = 0
		f.remainder = nil
		f.asm.reset()
	}
	if size == f.offset {
		return false, nil
	}

	if maxRead <= 0 {
		maxRead = 4 << 20
	}
	want := size - f.offset
	if want > maxRead {
		want = maxRead
	}

	fh, err := os.Open(f.path)
	if err != nil {
		return false, err
	}
	defer fh.Close()

	if _, err := fh.Seek(f.offset, io.SeekStart); err != nil {
		return false, err
	}

	buf := make([]byte, want)
	got, err := io.ReadFull(fh, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return false, err
	}
	if got == 0 {
		return false, nil
	}
	f.offset += int64(got)

	// 上一轮没成行的尾巴接上
	data := make([]byte, 0, len(f.remainder)+got)
	data = append(data, f.remainder...)
	data = append(data, buf[:got]...)

	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := data[:i]
		data = data[i+1:]
		if len(line) == 0 {
			continue
		}

		var r rawEntry
		if err := json.Unmarshal(line, &r); err != nil {
			emit(Entry{Stream: "raw", Log: string(line)})
			continue
		}
		f.asm.Feed(r, emit)
	}

	f.remainder = data

	// 单行超限（没有换行的畸形文件）强制吐出，否则内存会一直涨。
	if len(f.remainder) > maxLineBytes {
		emit(Entry{Stream: "raw", Log: string(f.remainder)})
		f.remainder = nil
	}

	return true, nil
}

// Flush 输出尚未收尾的 partial 片段，用于连接断开前的收尾。
func (f *Follower) Flush(emit func(Entry)) {
	f.asm.Flush(emit)
}
