package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogFileAppendAndReopen(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 9, 10, 30, 0, 0, time.Local)
	lf, err := openLogFile(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if base := filepath.Base(lf.Path()); base != "imgsim_20261109.log" {
		t.Fatalf("日志文件名应按天：%s", base)
	}
	lf.writeBatch([]string{"a", "b\r\n"})
	lf.writeBatch([]string{"c\n"})
	if err := lf.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "imgsim_20261109.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "a\nb\nc\n" {
		t.Fatalf("批量写入/换行规整不符：%q", got)
	}

	// 同一天重新打开应追加而不是覆盖。
	lf2, err := openLogFile(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	lf2.writeBatch([]string{"d"})
	_ = lf2.Close()
	raw, _ = os.ReadFile(lf2.Path())
	if got := string(raw); got != "a\nb\nc\nd\n" {
		t.Fatalf("重新打开应追加：%q", got)
	}
}

func TestLogFileNilSafe(t *testing.T) {
	var lf *logFile
	lf.writeBatch([]string{"x"}) // 不应 panic
	if lf.Path() != "" {
		t.Fatal("nil logFile 的 Path 应为空")
	}
	if err := lf.Close(); err != nil {
		t.Fatalf("nil logFile Close 应安全：%v", err)
	}
}

// 退出路径：把 buffer 里尚未刷新的日志一次性落盘，且不触碰 UI。
func TestDrainLogsToFile(t *testing.T) {
	dir := t.TempDir()
	lf, err := openLogFile(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a := &App{logs: newLogBuffer(), logFile: lf}
	a.appendLog("第一行", 0)
	a.appendLog("第二行", 1)
	a.drainLogsToFile()
	_ = lf.Close()

	raw, err := os.ReadFile(lf.Path())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "· 第一行") || !strings.Contains(text, "! 第二行") {
		t.Fatalf("日志应带级别前缀写入文件：%q", text)
	}
	if strings.Count(text, "\n") != 2 {
		t.Fatalf("应写入两行：%q", text)
	}
}

// 生产路径：initLogFile + appendLog + flushPending 应把同一批日志写进文件。
func TestFlushPendingWritesLogFile(t *testing.T) {
	dir := t.TempDir()
	a := &App{logs: newLogBuffer(), logsDir: filepath.Join(dir, "logs")}
	a.initLogFile()
	if a.logFile == nil {
		t.Fatal("应成功打开日志文件")
	}
	a.appendLog("批量落盘测试", 0)
	a.flushPending()
	a.flushPending() // 第二批为空，不应重复写入
	_ = a.logFile.Close()

	raw, err := os.ReadFile(a.logFile.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "批量落盘测试") {
		t.Fatalf("flushPending 应把日志写入文件：%q", raw)
	}
	if strings.Count(string(raw), "批量落盘测试") != 1 {
		t.Fatalf("同一批日志不应重复写入：%q", raw)
	}
}

// 端到端：worker 日志 -> App 缓冲 -> 日志文件，含张数上限的停止原因。
func TestEndToEndLogFileWithCountLimit(t *testing.T) {
	dir := t.TempDir()
	a := &App{logs: newLogBuffer(), logsDir: filepath.Join(dir, "logs")}
	a.initLogFile()
	if a.logFile == nil {
		t.Fatal("应成功打开日志文件")
	}
	defer a.logFile.Close()

	w := NewWorker(a.onWorkerLog, nil)
	if err := w.Start(RunConfig{
		Width: 64, Height: 48, TargetBytes: 16 * 1024,
		Format: FmtPNG, Pad: true, Interval: minInterval,
		Prefix: "E2E", OutDir: filepath.Join(dir, "out"), MaxCount: 2,
	}); err != nil {
		t.Fatal(err)
	}
	waitWorkerStopped(t, w, 5*time.Second)
	a.flushPending()
	a.drainLogsToFile()

	raw, err := os.ReadFile(a.logFile.Path())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "已达到生成张数上限（2 张）") {
		t.Fatalf("停止原因应进入日志文件：%s", text)
	}
	if got := strings.Count(text, "已达到生成张数上限"); got != 1 {
		t.Fatalf("停止原因应只出现一次，实际 %d：%s", got, text)
	}
	t.Logf("日志文件内容：\n%s", text)
}

// 日志文件写满一批后仍能继续追加（模拟运行中多次批量刷新）。
func TestLogFileMultipleBatches(t *testing.T) {
	dir := t.TempDir()
	lf, err := openLogFile(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		lf.writeBatch([]string{"line"})
	}
	_ = lf.Close()
	raw, err := os.ReadFile(lf.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "line\n"); got != 5 {
		t.Fatalf("应有 5 行，实际 %d：%q", got, raw)
	}
}
