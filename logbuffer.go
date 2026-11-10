package main

import (
	"strings"
	"sync"
)

const (
	maxLogLines  = 500 // 界面 TextEdit 最多保留的行数
	keepLogLines = 120 // 触发截断后保留的行数：越小，单次重建越轻
	lineBreak    = "\r\n"
)

// logBuffer 把「任意线程产生日志」与「后台落盘 / UI 批量刷新」解耦。
// push 时同一行分别进入两个独立队列：文件队列由后台 goroutine 消费（磁盘
// IO 不占 UI 线程），界面队列由 UI 线程一次性取空合并写入（即使 UI 曾经
// 繁忙而积压，恢复后也只做一次 TextEdit 更新，不会连续重绘造成卡顿）。
type logBuffer struct {
	mu        sync.Mutex
	pending   []string // 待写文件
	uiPending []string // 待刷界面
	lines     []string // 当前界面上的历史行
}

func newLogBuffer() *logBuffer { return &logBuffer{} }

// push 可从任意 goroutine 调用。
func (b *logBuffer) push(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, line)
	b.uiPending = append(b.uiPending, line)
}

// take 取出并清空「待写文件」批次，供后台 goroutine 落盘。
func (b *logBuffer) take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	batch := b.pending
	b.pending = nil
	return batch
}

// takeUI 取出并清空「待刷界面」批次，供 UI 线程合并成一次写入。
func (b *logBuffer) takeUI() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	batch := b.uiPending
	b.uiPending = nil
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

// clear 丢弃历史与两个队列里尚未刷新的日志。
func (b *logBuffer) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = nil
	b.uiPending = nil
	b.lines = nil
}

func (b *logBuffer) lineCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.lines)
}
