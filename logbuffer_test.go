package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestLogBufferKeepsEveryLine(t *testing.T) {
	b := newLogBuffer()
	const n = 120
	for i := 0; i < n; i++ {
		b.push(fmt.Sprintf("line-%d", i))
	}
	batch := b.take()
	if len(batch) != n {
		t.Fatalf("节流不得丢行：期望 %d，实际 %d", n, len(batch))
	}
	text, replace := b.flush(b.takeUI())
	if replace {
		t.Fatal("未超上限时不应整体重建")
	}
	if got := strings.Count(text, lineBreak); got != n {
		t.Fatalf("期望 %d 行，实际 %d 行", n, got)
	}
	if b.lineCount() != n {
		t.Fatalf("历史行数应为 %d，实际 %d", n, b.lineCount())
	}
	if len(b.take()) != 0 {
		t.Fatal("take 后队列应为空")
	}
}

func TestLogBufferTruncatesOldLines(t *testing.T) {
	b := newLogBuffer()
	const n = maxLogLines + 10
	for i := 0; i < n; i++ {
		b.push(fmt.Sprintf("L%d", i))
	}
	text, replace := b.flush(b.takeUI())
	if !replace {
		t.Fatal("超过上限时应返回 replace=true")
	}
	if b.lineCount() != keepLogLines {
		t.Fatalf("截断后应保留 %d 行，实际 %d", keepLogLines, b.lineCount())
	}
	lines := strings.Split(strings.TrimSuffix(text, lineBreak), lineBreak)
	if len(lines) != keepLogLines {
		t.Fatalf("重建文本应为 %d 行，实际 %d", keepLogLines, len(lines))
	}
	if want := fmt.Sprintf("L%d", n-keepLogLines); lines[0] != want {
		t.Fatalf("重建文本首行应为 %q，实际 %q", want, lines[0])
	}
}

func TestLogBufferClear(t *testing.T) {
	b := newLogBuffer()
	b.push("old-1")
	b.push("old-2")
	b.clear()
	if len(b.take()) != 0 || len(b.takeUI()) != 0 {
		t.Fatal("清空后文件队列与界面队列都应为空")
	}
	b.push("new")
	text, replace := b.flush(b.takeUI())
	if replace || !strings.Contains(text, "new") || strings.Contains(text, "old") {
		t.Fatalf("清空后不应残留旧日志：text=%q replace=%v", text, replace)
	}
	if b.lineCount() != 1 {
		t.Fatalf("清空后只应有 1 行，实际 %d", b.lineCount())
	}
}

// 文件队列（后台落盘）与界面队列（UI 刷新）相互独立，同一批日志两边都能拿到，
// 任一边先取走都不影响另一边。
func TestLogBufferSeparateFileAndUIQueues(t *testing.T) {
	b := newLogBuffer()
	b.push("one")
	b.push("two")

	fileBatch := b.take()
	uiBatch := b.takeUI()
	if len(fileBatch) != 2 || len(uiBatch) != 2 {
		t.Fatalf("两个队列都应有 2 行：file=%d ui=%d", len(fileBatch), len(uiBatch))
	}
	for i := range fileBatch {
		if fileBatch[i] != uiBatch[i] {
			t.Fatalf("第 %d 行不一致：file=%q ui=%q", i, fileBatch[i], uiBatch[i])
		}
	}
	if len(b.take()) != 0 || len(b.takeUI()) != 0 {
		t.Fatal("两个队列取出后都应为空")
	}
}

// UI 线程曾被占用而积压多个批次时，恢复后一次性取走全部，只做一次界面写入，
// 不会连续触发多次 TextEdit 重绘（周期性卡顿的根因）。
func TestLogBufferUIBatchMergesBacklog(t *testing.T) {
	b := newLogBuffer()
	for i := 0; i < 5; i++ {
		b.push(fmt.Sprintf("batch-%d", i))
		_ = b.take() // 模拟后台泵已落盘，但界面一直没刷新
	}
	ui := b.takeUI()
	if len(ui) != 5 {
		t.Fatalf("积压的 5 行应合并成一批，实际 %d", len(ui))
	}
	text, replace := b.flush(ui)
	if replace || strings.Count(text, lineBreak) != 5 {
		t.Fatalf("合并后应一次写入 5 行且不触发重建：replace=%v text=%q", replace, text)
	}
	if b.lineCount() != 5 {
		t.Fatalf("历史行数应为 5，实际 %d", b.lineCount())
	}
	if len(b.takeUI()) != 0 {
		t.Fatal("界面队列取出后应为空")
	}
}
