package main

import (
	"strings"
	"testing"
	"time"
)

// 回归「预计文件大小随参数实时变化」+「相同参数跳过重算」。
// lxn/walk 在 GroupBox 内不转发 EN_CHANGE，LineEdit.OnTextChanged 不可靠；
// 生产里由 startEstimatePump 每 120ms 在 UI 线程调用 scheduleEstimate。
// 测试里没有消息循环，因此直接模拟这一调用，验证参数一变数字立即改变。
// 无桌面环境下创建窗口会失败，此时跳过（不阻塞纯逻辑测试）。
//
// 注意：只创建一个窗口且刻意不调用 Dispose —— walk 的布局协程在没有消息
// 循环的 go test 进程里销毁窗口会触发库内部的退出竞态。
// 同一进程内只创建一个窗口：walk 的全局布局协程在 -count=N 时重复创建窗口会死锁。
var estimateUIWindowCreated bool

func TestEstimateUIRealtimeRefresh(t *testing.T) {
	if estimateUIWindowCreated {
		t.Skip("同一进程内只创建一个测试窗口")
	}
	a := &App{logs: newLogBuffer(), cfg: defaultConfig(t.TempDir())}
	if err := a.buildUI(); err != nil {
		t.Skipf("无法创建测试窗口：%v", err)
	}
	estimateUIWindowCreated = true
	a.applyConfigToUI()

	refresh := func() string {
		a.scheduleEstimate() // == 轮询器在 UI 线程里做的事
		return a.lblEstimate.Text()
	}
	base := refresh()
	if !strings.Contains(base, "预计文件") {
		t.Fatalf("启动后应立即显示单值预测，实际 %q", base)
	}

	// 相同参数快照应命中缓存，不重复计算。
	firstSpec := a.lastEstimateSpec
	_ = a.lblEstimate.SetText("被改写")
	a.scheduleEstimate()
	if !strings.Contains(a.lblEstimate.Text(), "被改写") || a.lastEstimateSpec != firstSpec {
		t.Fatal("相同参数应命中缓存、跳过重算")
	}

	if err := a.edQuality.SetText("20"); err != nil {
		t.Fatal(err)
	}
	q20 := refresh()
	if err := a.edW.SetText("640"); err != nil {
		t.Fatal(err)
	}
	w640 := refresh()
	if err := a.edSize.SetText("2000"); err != nil {
		t.Fatal(err)
	}
	bigTarget := refresh()
	a.cbPad.SetChecked(false)
	noPad := refresh()

	if base == q20 || q20 == w640 || w640 == bigTarget || bigTarget == noPad {
		t.Fatalf("参数变化必须让显示值实时改变：\n  默认 %q\n  质量20 %q\n  宽640 %q\n  目标2000 %q\n  不填充 %q",
			base, q20, w640, bigTarget, noPad)
	}
	if !strings.Contains(noPad, "预计文件") || strings.Contains(noPad, "填充 ") {
		t.Fatalf("关闭填充后不应再出现填充量：%q", noPad)
	}

	start := time.Now()
	a.edQuality.SetText("55")
	a.scheduleEstimate()
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("参数变化后的重算应保持在 UI 线程可接受的耗时内")
	}
	t.Logf("实时刷新：默认=%s | 质量20=%s | 宽640=%s | 目标2000=%s | 不填充=%s",
		base, q20, w640, bigTarget, noPad)
}
