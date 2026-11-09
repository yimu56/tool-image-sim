package main

import (
	"bufio"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// logFile 把运行日志按天追加写入 <dir>/imgsim_YYYYMMDD.log。
//
// 与界面日志共用同一条批量链路：UI 线程每 100ms 取出一批，一次写入文件并
// Flush。这样既保证每张图片一行不缺，又不会逐行刷盘拖慢高频生成。
// 文件日志不受界面「清空日志」与 500 行截断影响，便于事后追溯整轮压测。
type logFile struct {
	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	path string
}

// openLogFile 打开（或追加）当天的日志文件；目录不存在时自动创建。
func openLogFile(dir string, now time.Time) (*logFile, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "imgsim_"+now.Format("20060102")+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &logFile{f: f, w: bufio.NewWriterSize(f, 32*1024), path: path}, nil
}

func (l *logFile) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// writeBatch 追加一批日志行（可带换行，会被规整为单个换行）。
// 可从任意 goroutine 调用；nil 接收者安全。
func (l *logFile) writeBatch(lines []string) {
	if l == nil || len(lines) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil {
		return
	}
	for _, line := range lines {
		line = trimLineBreak(line)
		if line == "" {
			continue
		}
		_, _ = l.w.WriteString(line)
		_ = l.w.WriteByte('\n')
	}
	_ = l.w.Flush()
}

// Close 刷新缓冲并关闭文件；可重复调用。
func (l *logFile) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w != nil {
		_ = l.w.Flush()
		l.w = nil
	}
	if l.f != nil {
		err := l.f.Close()
		l.f = nil
		return err
	}
	return nil
}

// trimLineBreak 去掉行尾的 \r\n / \n / \r。
func trimLineBreak(s string) string {
	for len(s) > 0 {
		switch s[len(s)-1] {
		case '\n', '\r':
			s = s[:len(s)-1]
		default:
			return s
		}
	}
	return s
}
