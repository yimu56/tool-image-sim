package main

import (
	"strings"
	"sync"
)

const (
	maxLogLines  = 500 // 超过后触发一次重建，避免 TextEdit 无限增长拖慢界面
	keepLogLines = 250
	lineBreak    = "\r\n"
)

// logBuffer 把「任意线程产生日志」与「UI 线程批量刷新」解耦：
// worker 只做 O(1) 入队，UI 每 100ms 取一次批次，一次 AppendText 写完。
// 这样既能做到每张图片一行日志不丢，又不会在高频生成时把界面刷卡。
type logBuffer struct {
	mu      sync.Mutex
	pending []string
	lines   []string
}

func newLogBuffer() *logBuffer { return &logBuffer{} }

// push 可从任意 goroutine 调用。
func (b *logBuffer) push(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, line)
}

// take 取出并清空待刷新批次。
func (b *logBuffer) take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	batch := b.pending
	b.pending = nil
	return batch
}

// flush 把一批日志合入历史，返回要写入 TextEdit 的文本。
// replace=true 表示历史已被截断，需要用 SetText 整体重建而不是 AppendText。
func (b *logBuffer) flush(batch []string) (text string, replace bool) {
	if len(batch) == 0 {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, batch...)
	if len(b.lines) > maxLogLines {
		b.lines = append([]string(nil), b.lines[len(b.lines)-keepLogLines:]...)
		return strings.Join(b.lines, lineBreak) + lineBreak, true
	}
	return strings.Join(batch, lineBreak) + lineBreak, false
}

// clear 丢弃历史与尚未刷新的日志。
func (b *logBuffer) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = nil
	b.lines = nil
}

func (b *logBuffer) lineCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.lines)
}
