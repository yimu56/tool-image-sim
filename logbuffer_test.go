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
	text, replace := b.flush(batch)
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
	text, replace := b.flush(b.take())
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
	if len(b.take()) != 0 {
		t.Fatal("清空后待刷新队列应为空")
	}
	b.push("new")
	text, replace := b.flush(b.take())
	if replace || !strings.Contains(text, "new") || strings.Contains(text, "old") {
		t.Fatalf("清空后不应残留旧日志：text=%q replace=%v", text, replace)
	}
	if b.lineCount() != 1 {
		t.Fatalf("清空后只应有 1 行，实际 %d", b.lineCount())
	}
}
