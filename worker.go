package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const minInterval = 10 * time.Millisecond

// RunConfig 是一次运行用的完整参数快照。
type RunConfig struct {
	Width       int
	Height      int
	TargetBytes int64
	Format      string
	Pad         bool
	Quality     int
	AutoQuality bool
	Interval    time.Duration
	Prefix      string
	OutDir      string
	DateSubdir  bool
	Style       Style // 零值 / original 为原始风格
	// 运行限制：0 表示不限；两者都非 0 时满足任一即自动停止。
	MaxDuration time.Duration
	MaxCount    int64
}

// Stats 是运行时统计。
type Stats struct {
	Gen      int64
	Fail     int64
	Bytes    int64
	LastDur  time.Duration
	Interval time.Duration
}

// Worker 负责按固定频率产出图片。
type Worker struct {
	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
	doneCh  chan struct{}
	seq     int64
	stats   Stats

	onLog  func(level int, msg string)
	onStat func(s Stats)
}

func NewWorker(onLog func(int, string), onStat func(Stats)) *Worker {
	return &Worker{onLog: onLog, onStat: onStat}
}

func (w *Worker) Running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

func (w *Worker) Start(cfg RunConfig) error {
	if cfg.Interval < minInterval {
		cfg.Interval = minInterval
	}
	if cfg.OutDir == "" {
		return errors.New("输出目录不能为空")
	}

	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return errors.New("任务已在运行")
	}
	w.running = true
	w.stopCh = make(chan struct{})
	w.doneCh = make(chan struct{})
	w.stats = Stats{Interval: cfg.Interval}
	stop := w.stopCh
	w.mu.Unlock()

	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		close(w.doneCh) // 循环未启动，让 WaitStopped 立即返回
		return fmt.Errorf("创建输出目录失败: %w", err)
	}

	go w.loop(cfg, stop)
	return nil
}

func (w *Worker) Stop() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	ch := w.stopCh
	w.mu.Unlock()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// WaitStopped 等待采集循环真正退出（用于退出前把最后几行日志落盘）。
// timeout <= 0 时最多等 3 秒，避免大图收尾时长时间卡住进程退出。
func (w *Worker) WaitStopped(timeout time.Duration) {
	w.mu.Lock()
	ch := w.doneCh
	w.mu.Unlock()
	if ch == nil {
		return
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	select {
	case <-ch:
	case <-time.After(timeout):
	}
}

func (w *Worker) loop(cfg RunConfig, stop <-chan struct{}) {
	reason := ""
	defer func() {
		w.mu.Lock()
		w.running = false
		st := w.stats
		w.mu.Unlock()

		if reason == "" {
			w.logf(0, "任务已停止")
		} else {
			w.logf(0, "任务已停止："+reason)
		}
		// 自动停止时也要刷新一次界面，让「开始」按钮恢复可用。
		if w.onStat != nil {
			w.onStat(st)
		}
		close(w.doneCh)
	}()

	start := time.Now()
	var deadline <-chan time.Time
	if cfg.MaxDuration > 0 {
		tm := time.NewTimer(cfg.MaxDuration)
		defer tm.Stop()
		deadline = tm.C
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	// limitHit 返回空串表示未触发限制。张数按「已尝试生成数」计，
	// 正常情况下与落盘文件数一致；异常时也能保证不会无限重试。
	limitHit := func() string {
		if cfg.MaxCount > 0 && w.attempts() >= cfg.MaxCount {
			return fmt.Sprintf("已达到生成张数上限（%d 张）", cfg.MaxCount)
		}
		if cfg.MaxDuration > 0 && time.Since(start) >= cfg.MaxDuration {
			return fmt.Sprintf("已达到运行时长上限（%s）", formatDurationCN(cfg.MaxDuration))
		}
		return ""
	}

	for {
		if r := limitHit(); r != "" {
			reason = r
			return
		}
		w.generate(cfg) // 启动立刻出第一张，便于确认链路正常
		if r := limitHit(); r != "" {
			reason = r
			return
		}
		select {
		case <-stop:
			return
		case <-deadline:
			reason = fmt.Sprintf("已达到运行时长上限（%s）", formatDurationCN(cfg.MaxDuration))
			return
		case <-ticker.C:
		}
	}
}

// attempts 返回已尝试生成的张数（含失败）。
func (w *Worker) attempts() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seq
}

// formatDurationCN 把时长转成日志里易读的中文描述。
func formatDurationCN(d time.Duration) string {
	if d <= 0 {
		return "0 秒"
	}
	if d < time.Second {
		return fmt.Sprintf("%d 毫秒", d.Milliseconds())
	}
	sec := int64(d / time.Second)
	switch {
	case sec < 60:
		return fmt.Sprintf("%d 秒", sec)
	case sec < 3600:
		if sec%60 == 0 {
			return fmt.Sprintf("%d 分钟", sec/60)
		}
		return fmt.Sprintf("%d 分 %d 秒", sec/60, sec%60)
	default:
		h, m := sec/3600, (sec%3600)/60
		if m == 0 {
			return fmt.Sprintf("%d 小时", h)
		}
		return fmt.Sprintf("%d 小时 %d 分钟", h, m)
	}
}

func (w *Worker) generate(cfg RunConfig) {
	start := time.Now()

	w.mu.Lock()
	w.seq++
	seq := w.seq
	w.mu.Unlock()

	spec := Spec{
		Width:       cfg.Width,
		Height:      cfg.Height,
		TargetBytes: cfg.TargetBytes,
		Format:      cfg.Format,
		Pad:         cfg.Pad,
		Quality:     cfg.Quality,
		AutoQuality: cfg.AutoQuality,
		Seed:        frameSeed(start.UnixNano(), seq),
		Style:       cfg.Style,
	}

	res, err := Build(spec)
	if err != nil {
		w.bumpFail()
		w.logf(2, fmt.Sprintf("生成失败（第 %d 张）：%v", seq, err))
		return
	}

	dir := cfg.OutDir
	if cfg.DateSubdir {
		dir = filepath.Join(dir, start.Format("20060102"))
	}
	name := fmt.Sprintf("%s_%s_%06d.%s",
		sanitizePrefix(cfg.Prefix), start.Format("20060102_150405.000"), seq, extOf(cfg.Format))
	path := filepath.Join(dir, name)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.bumpFail()
		w.logf(2, fmt.Sprintf("创建目录失败：%v", err))
		return
	}
	if err := os.WriteFile(path, res.Data, 0o644); err != nil {
		w.bumpFail()
		w.logf(2, fmt.Sprintf("写入失败：%v", err))
		return
	}

	dur := time.Since(start)

	w.mu.Lock()
	w.stats.Gen++
	w.stats.Bytes += int64(len(res.Data))
	w.stats.LastDur = dur
	st := w.stats
	w.mu.Unlock()

	detail := ""
	if res.Note != "" {
		detail = " | " + res.Note
	} else if res.Padded {
		detail = " | 已填充至精确大小"
	}
	qualityInfo := ""
	if normalizeFormat(cfg.Format) == FmtJPG {
		qualityInfo = fmt.Sprintf("/质量 %d", res.Quality)
	}
	// 风格标记只在非原始风格时追加，原始风格的日志文本保持原样。
	styleTag := ""
	if res.Transformed {
		styleTag = fmt.Sprintf(" 风格 %s", res.Style)
	}
	// 内容指纹：即便细节档位/文件大小完全一样，也能一眼看出每帧画面不同
	sum := sha256.Sum256(res.Data)
	fp := hex.EncodeToString(sum[:4])
	w.logf(0, fmt.Sprintf("[%d] %s  %.1f KB  耗时 %dms  (细节 %.2f%s%s)  #%s%s",
		st.Gen, name, float64(len(res.Data))/1024.0, dur.Milliseconds(),
		res.Detail, qualityInfo, styleTag, fp, detail))

	if cfg.Interval > 0 && dur > cfg.Interval {
		w.logf(1, fmt.Sprintf("单张耗时 %dms 超过间隔 %dms，实际频率会低于设定值",
			dur.Milliseconds(), cfg.Interval.Milliseconds()))
	}
	if w.onStat != nil {
		w.onStat(st)
	}
}

func (w *Worker) bumpFail() {
	w.mu.Lock()
	w.stats.Fail++
	st := w.stats
	w.mu.Unlock()
	if w.onStat != nil {
		w.onStat(st)
	}
}

func (w *Worker) logf(level int, msg string) {
	if w.onLog != nil {
		w.onLog(level, msg)
	}
}

// frameSeed 把「纳秒时间戳」和「递增序号」混合成每帧唯一的随机种子。
// 只依赖时间戳会在极高频/时钟回拨时碰撞，加入黄金比例乘法后即使同一纳秒也不同。
func frameSeed(unixNano int64, seq int64) int64 {
	return int64(uint64(unixNano) ^ (uint64(seq) * 0x9E3779B97F4A7C15))
}

// sanitizePrefix 去掉文件名非法字符，避免写盘失败。
func sanitizePrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "IMG"
	}
	bad := `<>:"/\|?*`
	out := []rune(p)
	for i, r := range out {
		if strings.ContainsRune(bad, r) || r < 32 {
			out[i] = '_'
		}
	}
	s := strings.Trim(string(out), " .")
	if s == "" {
		return "IMG"
	}
	return s
}
