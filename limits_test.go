package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitWorkerStopped 轮询等待 worker 结束（不依赖内部实现细节）。
func waitWorkerStopped(t *testing.T, w *Worker, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !w.Running() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("worker 未在超时内停止")
}

func newLogCollector() (*[]string, *sync.Mutex) {
	logs := &[]string{}
	var mu sync.Mutex
	return logs, &mu
}

func collectedLogs(logs *[]string, mu *sync.Mutex) string {
	mu.Lock()
	defer mu.Unlock()
	return strings.Join(*logs, "\n")
}

func TestWorkerStopsAtMaxCount(t *testing.T) {
	dir := t.TempDir()
	logs, mu := newLogCollector()
	w := NewWorker(func(level int, msg string) {
		mu.Lock()
		*logs = append(*logs, msg)
		mu.Unlock()
	}, nil)

	cfg := RunConfig{
		Width: 64, Height: 48, TargetBytes: 16 * 1024,
		Format: FmtPNG, Pad: true, Interval: minInterval,
		Prefix: "LIM", OutDir: dir, MaxCount: 3,
	}
	if err := w.Start(cfg); err != nil {
		t.Fatal(err)
	}
	waitWorkerStopped(t, w, 5*time.Second)

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("张数上限 3 应只产出 3 个文件，实际 %d", len(files))
	}
	if got := collectedLogs(logs, mu); !strings.Contains(got, "已达到生成张数上限（3 张）") {
		t.Fatalf("停止原因应写入日志：%s", got)
	}
}

func TestWorkerStopsAtMaxDuration(t *testing.T) {
	dir := t.TempDir()
	logs, mu := newLogCollector()
	w := NewWorker(func(level int, msg string) {
		mu.Lock()
		*logs = append(*logs, msg)
		mu.Unlock()
	}, nil)

	cfg := RunConfig{
		Width: 64, Height: 48, TargetBytes: 16 * 1024,
		Format: FmtPNG, Pad: true, Interval: 20 * time.Millisecond,
		Prefix: "DUR", OutDir: dir, MaxDuration: 150 * time.Millisecond,
	}
	start := time.Now()
	if err := w.Start(cfg); err != nil {
		t.Fatal(err)
	}
	waitWorkerStopped(t, w, 5*time.Second)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("时长上限应在约 150ms 后停止，实际 %v", elapsed)
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("停止前应至少产出一张")
	}
	if got := collectedLogs(logs, mu); !strings.Contains(got, "已达到运行时长上限") {
		t.Fatalf("停止原因应写入日志：%s", got)
	}
}

func TestWorkerManualStopHasNoLimitReason(t *testing.T) {
	dir := t.TempDir()
	logs, mu := newLogCollector()
	w := NewWorker(func(level int, msg string) {
		mu.Lock()
		*logs = append(*logs, msg)
		mu.Unlock()
	}, nil)

	cfg := RunConfig{
		Width: 64, Height: 48, TargetBytes: 16 * 1024,
		Format: FmtPNG, Pad: true, Interval: 20 * time.Millisecond,
		Prefix: "MAN", OutDir: dir,
	}
	if err := w.Start(cfg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	w.Stop()
	w.WaitStopped(2 * time.Second)

	got := collectedLogs(logs, mu)
	if !strings.Contains(got, "任务已停止") {
		t.Fatalf("手动停止应记录日志：%s", got)
	}
	if strings.Contains(got, "上限") {
		t.Fatalf("手动停止不应带限制原因：%s", got)
	}
}

func TestFormatDurationCN(t *testing.T) {
	cases := map[time.Duration]string{
		0:                       "0 秒",
		500 * time.Millisecond:  "500 毫秒",
		1500 * time.Millisecond: "1 秒",
		60 * time.Second:        "1 分钟",
		90 * time.Second:        "1 分 30 秒",
		60 * time.Minute:        "1 小时",
		90 * time.Minute:        "1 小时 30 分钟",
	}
	for in, want := range cases {
		if got := formatDurationCN(in); got != want {
			t.Fatalf("formatDurationCN(%v)=%q，期望 %q", in, got, want)
		}
	}
}

func TestConfigLimitsDefaultPersistAndClamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := defaultConfig(dir)
	if cfg.LimitMinutes != 0 || cfg.LimitCount != 0 {
		t.Fatalf("默认应为不限：%d / %d", cfg.LimitMinutes, cfg.LimitCount)
	}

	cfg.LimitMinutes, cfg.LimitCount = 60, 1000
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	got := loadConfig(path, dir)
	if got.LimitMinutes != 60 || got.LimitCount != 1000 {
		t.Fatalf("限制应能持久化：%d / %d", got.LimitMinutes, got.LimitCount)
	}

	// 旧配置缺字段 → 0（不限）
	if err := os.WriteFile(path, []byte(`{"width":640,"height":480}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig(path, dir); got.LimitMinutes != 0 || got.LimitCount != 0 {
		t.Fatalf("缺字段应为不限：%d / %d", got.LimitMinutes, got.LimitCount)
	}

	// 负数 → 0
	if err := os.WriteFile(path, []byte(`{"limit_minutes":-5,"limit_count":-1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig(path, dir); got.LimitMinutes != 0 || got.LimitCount != 0 {
		t.Fatalf("负数应收敛为 0：%d / %d", got.LimitMinutes, got.LimitCount)
	}

	// 超范围 → 上限，避免 time.Duration 溢出
	if err := os.WriteFile(path, []byte(`{"limit_minutes":999999999,"limit_count":99999999999}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got = loadConfig(path, dir)
	if got.LimitMinutes != maxLimitMinutes || got.LimitCount != maxLimitCount {
		t.Fatalf("超范围应收敛到上限：%d / %d", got.LimitMinutes, got.LimitCount)
	}
}

func TestLimitText(t *testing.T) {
	if got := limitText(Config{}); got != "无限制" {
		t.Fatalf("无限制描述错误：%q", got)
	}
	got := limitText(Config{LimitMinutes: 60, LimitCount: 1000})
	if !strings.Contains(got, "1 小时") || !strings.Contains(got, "1000") {
		t.Fatalf("限制描述错误：%q", got)
	}
}
