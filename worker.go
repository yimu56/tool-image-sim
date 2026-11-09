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
	w.stats = Stats{Interval: cfg.Interval}
	stop := w.stopCh
	w.mu.Unlock()

	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
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

func (w *Worker) loop(cfg RunConfig, stop <-chan struct{}) {
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		w.logf(0, "任务已停止")
	}()

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	w.generate(cfg) // 启动立刻出第一张，便于确认链路正常
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			w.generate(cfg)
		}
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
