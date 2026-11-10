package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 每轮重新计数：第一轮生成 3 张，第二轮把上限改成 2 张，仍应正常出图；
// 文件名带轮数，末尾 6 位序号每轮从 000001 重新开始。
func TestWorkerResetsCountPerRound(t *testing.T) {
	dir := t.TempDir()
	logs, mu := newLogCollector()
	w := NewWorker(func(level int, msg string) {
		mu.Lock()
		*logs = append(*logs, msg)
		mu.Unlock()
	}, nil)

	base := RunConfig{
		Width: 64, Height: 48, TargetBytes: 16 * 1024,
		Format: FmtPNG, Pad: true, Interval: minInterval,
		Prefix: "RND", OutDir: dir,
	}

	first := base
	first.Round, first.MaxCount = 1, 3
	if err := w.Start(first); err != nil {
		t.Fatal(err)
	}
	w.WaitStopped(5 * time.Second)

	second := base
	second.Round, second.MaxCount = 2, 2
	if err := w.Start(second); err != nil {
		t.Fatal(err)
	}
	w.WaitStopped(5 * time.Second)

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 {
		t.Fatalf("两轮应共 5 个文件，实际 %d", len(files))
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name())
	}
	joined := strings.Join(names, "\n")
	for _, want := range []string{
		"_001_000001.", "_001_000002.", "_001_000003.",
		"_002_000001.", "_002_000002.",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少 %q：\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "_002_000003.") {
		t.Fatalf("第二轮不应沿用上一轮序号：\n%s", joined)
	}
	if got := strings.Count(collectedLogs(logs, mu), "已达到生成张数上限"); got != 2 {
		t.Fatalf("两轮应各自触发一次上限，实际 %d 次", got)
	}
	if w.Running() {
		t.Fatal("第二轮结束后不应仍在运行")
	}
}

// 连续快速重启：每一轮只关闭自己的 done channel，不会误关下一轮的，
// WaitStopped 不会提前返回，也不会出现「新一轮已经开始却拿不到文件」。
func TestWorkerConsecutiveRoundsCloseOwnDoneChannel(t *testing.T) {
	dir := t.TempDir()
	w := NewWorker(nil, nil)
	const rounds = 6
	for round := 1; round <= rounds; round++ {
		if err := w.Start(RunConfig{
			Width: 64, Height: 48, TargetBytes: 16 * 1024,
			Format: FmtPNG, Pad: true, Interval: minInterval,
			Prefix: "FAST", OutDir: dir, Round: round, MaxCount: 1,
		}); err != nil {
			t.Fatalf("第 %d 轮启动失败：%v", round, err)
		}
		w.WaitStopped(3 * time.Second)
		if w.Running() {
			t.Fatalf("第 %d 轮未结束", round)
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != rounds {
		t.Fatalf("应产出 %d 个文件，实际 %d", rounds, len(files))
	}
	for round := 1; round <= rounds; round++ {
		want := fmt.Sprintf("_%03d_000001.", round)
		found := false
		for _, f := range files {
			if strings.Contains(f.Name(), want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("第 %d 轮文件名缺少 %q", round, want)
		}
	}
}

// 轮数只存在于进程内存：Config 与 config.json 都不含该字段，重新构造 App
// （等价于重新打开软件）时轮数归零，第一次开始生成即为第 1 轮。
func TestRoundIsRuntimeOnly(t *testing.T) {
	if _, ok := reflect.TypeOf(Config{}).FieldByName("Round"); ok {
		t.Fatal("轮数不应进入 Config / config.json")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := defaultConfig(dir)
	cfg.LimitCount = 50
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["round"]; ok {
		t.Fatal("config.json 不应持久化轮数")
	}
	got := loadConfig(path, dir)
	if got.LimitCount != 50 {
		t.Fatalf("张数上限仍应持久化：%+v", got)
	}

	a := &App{}
	if a.round != 0 {
		t.Fatalf("重新打开软件轮数应归零，实际 %d", a.round)
	}
	if next := a.round + 1; next != 1 {
		t.Fatalf("第一次开始生成应为第 1 轮，实际 %d", next)
	}
}

// 序号每轮从 1 重新开始，因此种子额外混入轮数，跨轮同序号也不会共用种子。
func TestFrameSeedDistinctAcrossRounds(t *testing.T) {
	const nano = int64(1700000000000000000)
	seen := map[int64]bool{}
	for round := int64(1); round <= 5; round++ {
		for seq := int64(1); seq <= 10; seq++ {
			s := frameSeed(nano, seq, round)
			if seen[s] {
				t.Fatalf("第 %d 轮第 %d 张出现种子碰撞", round, seq)
			}
			seen[s] = true
		}
	}
	if frameSeed(nano, 1) == frameSeed(nano, 2) {
		t.Fatal("不传轮数的旧调用仍应保证不同序号种子不同")
	}
}
