package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

const (
	appTitle         = "机台图片模拟器 v1.2.0"
	logFlushInterval = 100 * time.Millisecond
)

type App struct {
	mw *walk.MainWindow

	edW, edH, edSize     *walk.LineEdit
	edQuality            *walk.LineEdit
	edInterval, edPrefix *walk.LineEdit
	edLimitMin           *walk.LineEdit
	edLimitCount         *walk.LineEdit
	edDir                *walk.LineEdit
	cbFormat             *walk.ComboBox
	cbStyle              *walk.ComboBox
	cbPad, cbDate        *walk.CheckBox
	cbAutoQuality        *walk.CheckBox
	btnStart, btnStop    *walk.PushButton
	btnClearLog          *walk.PushButton
	btnOpenLog           *walk.PushButton
	lblStat              *walk.Label
	lblEstimate          *walk.Label
	lblNatural           *walk.Label
	teLog                *walk.TextEdit

	cfg     Config
	cfgPath string
	worker  *Worker
	logs    *logBuffer
	logFile *logFile
	logsDir string
	// limitCount 只用于状态栏显示当前上限（0=不限）。
	limitCount int64
	// round 是「第几轮生成」：每次成功点击开始生成 +1，只在进程内存里累计，
	// 不写 config.json，重启程序后归零（新一轮从 001 开始）。
	round int
	// styleOverride 非空表示 -style 指定了风格：只影响本次运行，
	// 写回 config.json 时必须还原成磁盘上原有的 diskStyle。
	styleOverride string
	diskStyle     string

	// 预计体积：UI 线程同步计算（等比小采样，约 10~30ms），输入即变。
	// 参数快照相同则跳过，避免配置回填时重复计算。
	lastEstimateSpec  EstimateSpec
	lastEstimateValid bool

	// 状态栏：worker 回调只写快照，界面更新统一由 100ms 泵在 UI 线程完成，
	// 避免高频生成时多个 goroutine 争抢 Synchronize 消息队列。
	statMu    sync.Mutex
	statDirty bool
	lastStats Stats

	logMu  sync.Mutex
	closed bool
}

func main() {
	cfgPath := flag.String("config", "", "配置文件路径（默认 exe 同目录 config.json）")
	autoStart := flag.Bool("autostart", false, "启动后立即开始生成")
	once := flag.Bool("once", false, "只生成一张图片后退出（用于自检）")
	styleFlag := flag.String("style", "", "图片风格 original|minecraft（覆盖 config.json，默认 original）")
	minutesFlag := flag.Int("minutes", -1, "本次运行时长上限（分钟，0=不限；覆盖 config.json）")
	countFlag := flag.Int64("count", -1, "本次生成张数上限（0=不限；覆盖 config.json）")
	flag.Parse()

	exeDir := executableDir()
	if *cfgPath == "" {
		*cfgPath = filepath.Join(exeDir, "config.json")
	}

	app := &App{cfgPath: *cfgPath, logs: newLogBuffer(), logsDir: filepath.Join(exeDir, "logs")}
	// 风格解析告警在 GUI 里进运行日志，自检模式直接写终端（无需窗口）。
	if *once {
		styleWarnf = func(msg string) { fmt.Fprintln(os.Stderr, "[style]", msg) }
	} else {
		styleWarnf = func(msg string) { app.appendLog("[style] "+msg, 1) }
	}
	app.cfg = loadConfig(*cfgPath, exeDir)
	if *styleFlag != "" {
		app.diskStyle = app.cfg.Style // 配置文件里本来的默认值
		app.styleOverride = string(resolveStyle(*styleFlag))
		app.cfg.Style = app.styleOverride // 仅本次运行生效
	}
	if *minutesFlag >= 0 {
		app.cfg.LimitMinutes = *minutesFlag
	}
	if *countFlag >= 0 {
		app.cfg.LimitCount = *countFlag
	}
	app.worker = NewWorker(app.onWorkerLog, app.onWorkerStat)

	if *once {
		app.runOnce()
		return
	}

	if err := app.buildUI(); err != nil {
		walk.MsgBox(nil, appTitle, "界面创建失败："+err.Error(), walk.MsgBoxIconError)
		return
	}
	app.initLogFile()
	app.applyConfigToUI()
	app.fitWindow()
	app.setRunning(false)
	app.appendLog(fmt.Sprintf("程序就绪，输出目录：%s", app.cfg.OutDir), 0)
	if path := app.logFile.Path(); path != "" {
		app.appendLog("日志文件："+path, 0)
	}

	stopPump := make(chan struct{})
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		app.logPump(stopPump)
	}()
	if *autoStart {
		app.onStart()
	}

	app.mw.Run()

	close(stopPump)
	<-pumpDone // 等后台刷盘泵退出，避免与 logFile.Close 竞态丢日志
	app.worker.Stop()
	app.worker.WaitStopped(3 * time.Second) // 等采集循环写出最后几行
	app.drainLogsToFile()                   // 退出前把剩余日志落盘（不碰 UI）
	app.markClosed()
	_ = app.logFile.Close()
}

func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	return filepath.Dir(exe)
}

func (a *App) buildUI() error {
	return MainWindow{
		AssignTo: &a.mw,
		Title:    appTitle,
		MinSize:  Size{Width: 680, Height: 600},
		Layout:   VBox{Spacing: 8},
		Children: []Widget{
			GroupBox{
				Title:  "图片参数",
				Layout: Grid{Columns: 4, Spacing: 6},
				Children: []Widget{
					Label{Text: "宽度 (px)"},
					LineEdit{AssignTo: &a.edW, MaxLength: 5, MinSize: Size{Width: 90}, OnTextChanged: a.scheduleEstimate},
					Label{Text: "高度 (px)"},
					LineEdit{AssignTo: &a.edH, MaxLength: 5, MinSize: Size{Width: 90}, OnTextChanged: a.scheduleEstimate},

					Label{Text: "目标大小 (KB)"},
					LineEdit{AssignTo: &a.edSize, MaxLength: 7, MinSize: Size{Width: 90}, OnTextChanged: a.scheduleEstimate},
					Label{Text: "图片格式"},
					ComboBox{AssignTo: &a.cbFormat, Model: []string{FmtJPG, FmtPNG, FmtBMP}, MinSize: Size{Width: 90}, OnCurrentIndexChanged: a.scheduleEstimate},

					Label{Text: "图片风格"},
					ComboBox{AssignTo: &a.cbStyle, Model: StyleNames(), MinSize: Size{Width: 90}, OnCurrentIndexChanged: a.scheduleEstimate},
					Label{Text: "minecraft 为本地方块化转换，不联网", ColumnSpan: 2},

					Label{Text: "JPEG 质量 (1-100)"},
					LineEdit{AssignTo: &a.edQuality, MaxLength: 3, MinSize: Size{Width: 90}, OnTextChanged: a.scheduleEstimate},
					CheckBox{AssignTo: &a.cbAutoQuality, Text: "压不到目标时自动降质量", ColumnSpan: 2, OnCheckedChanged: a.scheduleEstimate},

					Label{Text: "预计文件大小"},
					Label{AssignTo: &a.lblEstimate, Text: "计算中…", ColumnSpan: 3},
					Label{Text: "满细节自然体积"},
					Label{AssignTo: &a.lblNatural, Text: "计算中…", ColumnSpan: 3},
					Label{Text: "图像数据受目标大小约束会自动降细节档位；满细节自然体积随分辨率变化", ColumnSpan: 4},

					CheckBox{AssignTo: &a.cbPad, Text: "严格填充到目标大小（字节数完全一致）", ColumnSpan: 2, OnCheckedChanged: a.scheduleEstimate},
					CheckBox{AssignTo: &a.cbDate, Text: "按日期建子目录 yyyyMMdd", ColumnSpan: 2},
				},
			},
			GroupBox{
				Title:  "生成设置",
				Layout: Grid{Columns: 4, Spacing: 6},
				Children: []Widget{
					Label{Text: "生成间隔 (ms)"},
					LineEdit{AssignTo: &a.edInterval, MaxLength: 7, MinSize: Size{Width: 90}},
					Label{Text: "文件名前缀"},
					LineEdit{AssignTo: &a.edPrefix, MaxLength: 29, MinSize: Size{Width: 90}},

					Label{Text: "运行时长 (分钟，0=不限)"},
					LineEdit{AssignTo: &a.edLimitMin, MaxLength: 6, MinSize: Size{Width: 90}},
					Label{Text: "生成张数 (0=不限)"},
					LineEdit{AssignTo: &a.edLimitCount, MaxLength: 10, MinSize: Size{Width: 90}},

					Label{Text: "输出目录"},
					Composite{
						ColumnSpan: 3,
						Layout:     HBox{MarginsZero: true, Spacing: 6},
						Children: []Widget{
							LineEdit{AssignTo: &a.edDir, MinSize: Size{Width: 280}},
							PushButton{Text: "浏览...", MinSize: Size{Width: 72}, OnClicked: a.onBrowse},
							PushButton{Text: "打开", MinSize: Size{Width: 60}, OnClicked: a.onOpenDir},
							HSpacer{},
						},
					},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 8},
				Children: []Widget{
					PushButton{AssignTo: &a.btnStart, Text: "开始生成", MinSize: Size{Width: 96}, OnClicked: a.onStart},
					PushButton{AssignTo: &a.btnStop, Text: "停止", MinSize: Size{Width: 80}, OnClicked: a.onStop},
					PushButton{AssignTo: &a.btnClearLog, Text: "清空日志", MinSize: Size{Width: 80}, OnClicked: a.onClearLog},
					PushButton{AssignTo: &a.btnOpenLog, Text: "打开日志", MinSize: Size{Width: 80}, OnClicked: a.onOpenLogs},
					HSpacer{},
					Label{AssignTo: &a.lblStat, Text: "就绪"},
				},
			},
			GroupBox{
				Title:  "运行日志（每张图片一行，最多保留最近 500 行）",
				Layout: VBox{},
				Children: []Widget{
					TextEdit{AssignTo: &a.teLog, VScroll: true, MinSize: Size{Height: 180}},
				},
			},
		},
	}.Create()
}

// fitWindow 按当前显示器工作区收敛窗口尺寸，避免高 DPI 下窗口超出屏幕。
func (a *App) fitWindow() {
	dpi := 96
	if d := a.mw.DPI(); d > 0 {
		dpi = d
	}
	sw := int(win.GetSystemMetrics(win.SM_CXSCREEN))
	sh := int(win.GetSystemMetrics(win.SM_CYSCREEN))

	w, h := 780, 720 // 逻辑尺寸
	if maxW := sw*96/dpi - 32; w > maxW && maxW > 400 {
		w = maxW
	}
	if maxH := sh*96/dpi - 90; h > maxH && maxH > 400 {
		h = maxH
	}
	_ = a.mw.SetSizePixels(walk.Size{Width: w * dpi / 96, Height: h * dpi / 96})
}

// ---------- 配置 <-> 界面 ----------

func (a *App) applyConfigToUI() {
	a.edW.SetText(strconv.Itoa(a.cfg.Width))
	a.edH.SetText(strconv.Itoa(a.cfg.Height))
	a.edSize.SetText(strconv.Itoa(a.cfg.TargetKB))
	a.edQuality.SetText(strconv.Itoa(a.cfg.Quality))
	a.edInterval.SetText(strconv.Itoa(a.cfg.IntervalMS))
	a.edPrefix.SetText(a.cfg.Prefix)
	a.edLimitMin.SetText(strconv.Itoa(a.cfg.LimitMinutes))
	a.edLimitCount.SetText(strconv.FormatInt(a.cfg.LimitCount, 10))
	a.edDir.SetText(a.cfg.OutDir)
	switch normalizeFormat(a.cfg.Format) {
	case FmtPNG:
		a.cbFormat.SetCurrentIndex(1)
	case FmtBMP:
		a.cbFormat.SetCurrentIndex(2)
	default:
		a.cbFormat.SetCurrentIndex(0)
	}
	a.cbStyle.SetCurrentIndex(indexOfStyle(Style(a.cfg.Style)))
	a.cbPad.SetChecked(a.cfg.Pad)
	a.cbDate.SetChecked(a.cfg.DateSubdir)
	a.cbAutoQuality.SetChecked(a.cfg.AutoQuality)
	a.teLog.SetReadOnly(true)
	a.scheduleEstimate() // 启动时按当前配置先算一次
}

func (a *App) readConfig() (Config, error) {
	c := a.cfg

	w, err := a.readInt(a.edW, "宽度")
	if err != nil {
		return c, err
	}
	if w < 8 || w > 16000 {
		return c, fmt.Errorf("宽度需在 8 ~ 16000 之间")
	}
	h, err := a.readInt(a.edH, "高度")
	if err != nil {
		return c, err
	}
	if h < 8 || h > 16000 {
		return c, fmt.Errorf("高度需在 8 ~ 16000 之间")
	}
	kb, err := a.readInt(a.edSize, "目标大小")
	if err != nil {
		return c, err
	}
	if kb < 1 || kb > 200*1024 {
		return c, fmt.Errorf("目标大小需在 1 ~ 204800 KB 之间")
	}
	quality, err := a.readInt(a.edQuality, "JPEG 质量")
	if err != nil {
		return c, err
	}
	if quality < 1 || quality > 100 {
		return c, fmt.Errorf("JPEG 质量需在 1 ~ 100 之间")
	}
	interval, err := a.readInt(a.edInterval, "生成间隔")
	if err != nil {
		return c, err
	}
	if interval < 10 || interval > 3600000 {
		return c, fmt.Errorf("生成间隔需在 10 ~ 3600000 毫秒之间")
	}
	limitMin, err := a.readInt(a.edLimitMin, "运行时长")
	if err != nil {
		return c, err
	}
	if limitMin < 0 || limitMin > maxLimitMinutes {
		return c, fmt.Errorf("运行时长需在 0 ~ %d 分钟之间（0=不限）", maxLimitMinutes)
	}
	limitCount, err := a.readInt(a.edLimitCount, "生成张数上限")
	if err != nil {
		return c, err
	}
	if limitCount < 0 || int64(limitCount) > maxLimitCount {
		return c, fmt.Errorf("生成张数需在 0 ~ %d 之间（0=不限）", maxLimitCount)
	}
	dir := strings.TrimSpace(a.edDir.Text())
	if dir == "" {
		return c, fmt.Errorf("输出目录不能为空")
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}

	c.Width, c.Height, c.TargetKB, c.IntervalMS = w, h, kb, interval
	c.LimitMinutes, c.LimitCount = limitMin, int64(limitCount)
	c.Quality = quality
	c.Format = normalizeFormat(a.cbFormat.Text())
	c.Style = string(resolveStyle(a.cbStyle.Text()))
	c.Pad = a.cbPad.Checked()
	c.DateSubdir = a.cbDate.Checked()
	c.AutoQuality = a.cbAutoQuality.Checked()
	c.Prefix = a.edPrefix.Text()
	c.OutDir = dir
	return c, nil
}

func (a *App) readInt(ed *walk.LineEdit, name string) (int, error) {
	s := strings.TrimSpace(ed.Text())
	if s == "" {
		return 0, fmt.Errorf("%s不能为空", name)
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s必须是整数：%s", name, s)
	}
	return v, nil
}

// ---------- 预计文件大小（UI 线程同步计算，输入即变） ----------

// estimateSpecFromUI 在 UI 线程把控件状态拍成一份完整快照。
func (a *App) estimateSpecFromUI() (EstimateSpec, bool) {
	if a.edW == nil || a.edH == nil || a.edSize == nil || a.edQuality == nil ||
		a.cbFormat == nil || a.cbStyle == nil || a.cbPad == nil ||
		a.cbAutoQuality == nil || a.lblEstimate == nil {
		return EstimateSpec{}, false // 界面构建早期，控件尚未齐全
	}
	w, e1 := strconv.Atoi(strings.TrimSpace(a.edW.Text()))
	h, e2 := strconv.Atoi(strings.TrimSpace(a.edH.Text()))
	kb, e3 := strconv.Atoi(strings.TrimSpace(a.edSize.Text()))
	q, e4 := strconv.Atoi(strings.TrimSpace(a.edQuality.Text()))
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return EstimateSpec{}, false
	}
	return EstimateSpec{
		Width: w, Height: h, Quality: q, TargetKB: kb,
		Format:      a.cbFormat.Text(),
		Style:       Style(a.cbStyle.Text()),
		Pad:         a.cbPad.Checked(),
		AutoQuality: a.cbAutoQuality.Checked(),
	}, true
}

// scheduleEstimate 由参数变化事件与 100ms UI 泵共同触发。估算只跑一张等比
// 小采样图（约 20~60ms），因此直接在 UI 线程同步完成并立即刷新 Label：
// 没有防抖、没有后台线程、没有跨线程同步，参数一变数字就变。
func (a *App) scheduleEstimate() {
	if a.lblEstimate == nil {
		return
	}
	spec, valid := a.estimateSpecFromUI()
	if !valid {
		a.lastEstimateValid = false
		_ = a.lblEstimate.SetText("参数不完整")
		a.setNaturalText("参数不完整")
		return
	}
	// 相同的参数快照不重复计算（配置回填会连续触发多次 TextChanged）。
	if a.lastEstimateValid && spec == a.lastEstimateSpec {
		return
	}
	a.lastEstimateSpec, a.lastEstimateValid = spec, true

	est, err := EstimateSize(spec)
	if err != nil {
		_ = a.lblEstimate.SetText("无法估算：" + err.Error())
		a.setNaturalText("—")
		return
	}
	_ = a.lblEstimate.SetText(est.Display())
	a.setNaturalText(est.NaturalDisplay())
}

func (a *App) setNaturalText(s string) {
	if a.lblNatural != nil {
		_ = a.lblNatural.SetText(s)
	}
}

// ---------- 按钮事件 ----------

// configForPersist 决定写回 config.json 的内容。override 为空时按界面选择
// 正常持久化；非空（-style 临时覆盖）时必须把 style 还原成磁盘上的旧值，
// 保证「默认风格」不会被一次命令行试跑或 -autostart 悄悄改写。
func configForPersist(cfg Config, override, diskStyle string) Config {
	if override == "" {
		return cfg
	}
	cfg.Style = diskStyle
	return cfg
}

func (a *App) onStart() {
	cfg, err := a.readConfig()
	if err != nil {
		walk.MsgBox(a.mw, "参数有误", err.Error(), walk.MsgBoxIconWarning)
		return
	}
	a.cfg = cfg
	// -style 只是本次运行的临时覆盖；写回配置时还原磁盘上原有的 style，
	// 否则「试一次 minecraft」就会把下次启动的默认风格一起改掉。
	if err := saveConfig(a.cfgPath, configForPersist(cfg, a.styleOverride, a.diskStyle)); err != nil {
		a.appendLog("保存配置失败："+err.Error(), 1)
	}

	round := a.round + 1
	run := RunConfig{
		Width:       cfg.Width,
		Height:      cfg.Height,
		TargetBytes: int64(cfg.TargetKB) * 1024,
		Format:      cfg.Format,
		Pad:         cfg.Pad,
		Quality:     cfg.Quality,
		AutoQuality: cfg.AutoQuality,
		Interval:    time.Duration(cfg.IntervalMS) * time.Millisecond,
		Prefix:      cfg.Prefix,
		OutDir:      cfg.OutDir,
		DateSubdir:  cfg.DateSubdir,
		Style:       Style(cfg.Style),
		Round:       round,
		MaxDuration: time.Duration(cfg.LimitMinutes) * time.Minute,
		MaxCount:    cfg.LimitCount,
	}
	a.limitCount = cfg.LimitCount
	if err := a.worker.Start(run); err != nil {
		walk.MsgBox(a.mw, "无法开始", err.Error(), walk.MsgBoxIconWarning)
		return
	}
	a.round = round // 启动失败不消耗轮数；轮数只存在于本次进程
	a.setRunning(true)
	q := ""
	if cfg.Format == FmtJPG {
		q = fmt.Sprintf(" 质量 %d（自动降质量 %s）", cfg.Quality, boolCN(cfg.AutoQuality))
	}
	styleInfo := ""
	if Style(cfg.Style) != StyleOriginal {
		styleInfo = fmt.Sprintf(" 风格 %s", cfg.Style)
	}
	a.appendLog(fmt.Sprintf("第 %d 轮开始生成：%dx%d %s%s%s 目标 %dKB 间隔 %dms → %s",
		round, cfg.Width, cfg.Height, cfg.Format, q, styleInfo, cfg.TargetKB, cfg.IntervalMS, cfg.OutDir), 0)
	if cfg.LimitMinutes > 0 || cfg.LimitCount > 0 {
		a.appendLog("运行限制："+limitText(cfg)+"（满足任一条件即自动停止，每轮从 0 重新计数）", 0)
	}
	if cfg.Format == FmtBMP {
		a.appendLog(fmt.Sprintf("BMP 不带压缩，实际大小由尺寸决定（约 %.0f KB），目标大小仅在大于该值时有效",
			float64(cfg.Width*cfg.Height)*3/1048576.0), 1)
	}
	if cfg.Format != FmtJPG {
		a.appendLog("当前格式为 "+cfg.Format+"，JPEG 质量参数不生效", 1)
	}
	if Style(cfg.Style) != StyleOriginal {
		a.appendLog(fmt.Sprintf("风格 %s：基于原始随机噪声图做本地转换（无联网 / 无 AI），尺寸、种子与生成间隔不变；方块化会提高压缩率，体积仍按原有档位探测 + 元数据填充逼近目标，缺口见每张图片备注", cfg.Style), 0)
	}
}

func boolCN(v bool) string {
	if v {
		return "开"
	}
	return "关"
}

// limitText 把运行限制拼成日志里的一行描述。
func limitText(cfg Config) string {
	var parts []string
	if cfg.LimitMinutes > 0 {
		parts = append(parts, "时长 "+formatDurationCN(time.Duration(cfg.LimitMinutes)*time.Minute))
	}
	if cfg.LimitCount > 0 {
		parts = append(parts, fmt.Sprintf("张数 %d", cfg.LimitCount))
	}
	if len(parts) == 0 {
		return "无限制"
	}
	return strings.Join(parts, " / ")
}

func (a *App) onStop() {
	a.worker.Stop()
	a.setRunning(false)
}

func (a *App) onClearLog() {
	a.flushPending() // 先落盘，避免尚未刷新的日志被一起丢弃
	a.logs.clear()
	a.teLog.SetText("")
	a.appendLog("日志已清空（仅清空界面显示，文件日志继续保留）", 0)
	a.flushPending() // 按钮在 UI 线程，可以立即刷新
}

func (a *App) onBrowse() {
	dlg := walk.FileDialog{Title: "选择图片输出目录"}
	ok, err := dlg.ShowBrowseFolder(a.mw)
	if err != nil {
		a.appendLog("选择目录失败："+err.Error(), 1)
		return
	}
	if ok && dlg.FilePath != "" {
		a.edDir.SetText(dlg.FilePath)
	}
}

func (a *App) onOpenDir() {
	a.openFolder(strings.TrimSpace(a.edDir.Text()))
}

func (a *App) onOpenLogs() {
	a.openFolder(a.logsDir)
}

// openFolder 在资源管理器里打开目录，不存在则先创建。
func (a *App) openFolder(dir string) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.appendLog("目录不存在且创建失败："+err.Error(), 1)
		return
	}
	cmd := exec.Command("explorer", dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}

func (a *App) setRunning(running bool) {
	if a.btnStart == nil || a.btnStop == nil {
		return
	}
	a.btnStart.SetEnabled(!running)
	a.btnStop.SetEnabled(running)
}

// ---------- 日志：后台落盘，UI 线程批量刷新 ----------

// logPump 是唯一的界面刷新泵：磁盘 IO 留在后台 goroutine，所有 UI 更新
// （日志、预计大小、状态栏）收敛到一次 Synchronize，避免多个 goroutine
// 争抢 UI 消息队列、以及队列积压后连续多次重绘造成的周期性卡顿。
func (a *App) logPump(stop <-chan struct{}) {
	tk := time.NewTicker(logFlushInterval)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			// 文件日志的磁盘 IO 不占用 UI 消息循环。
			a.logFile.writeBatch(a.logs.take())
			a.safeSync(func() {
				a.flushToUI()
				a.scheduleEstimate()
				a.flushStats()
			})
		}
	}
}

// flushToUI 只刷新界面控件，磁盘 IO 已由后台 logPump 完成。
// 一次取走全部待刷批次合并成一次写入：即使 UI 线程曾被占用而积压，恢复后
// 也只做一次 TextEdit 更新，不会连续触发多次重绘。
func (a *App) flushToUI() {
	batch := a.logs.takeUI()
	if len(batch) == 0 {
		return
	}
	text, replace := a.logs.flush(batch)
	if text == "" || a.teLog == nil {
		return // teLog 为 nil 只可能出现在无界面的测试/自检场景
	}
	if replace {
		// 截断需要整体重建文本，期间关掉重绘，避免 RichEdit 反复重排重绘。
		a.teLog.SendMessage(win.WM_SETREDRAW, 0, 0)
		_ = a.teLog.SetText(text)
		a.teLog.SendMessage(win.WM_SETREDRAW, 1, 0)
		_ = a.teLog.Invalidate()
	} else {
		a.teLog.AppendText(text)
	}
	a.scrollLogToEnd()
}

// flushPending 是 UI 线程/按钮/测试用的兼容入口：落盘 + 刷界面。
// 生产运行时磁盘落盘已由 logPump 在后台完成，这里取到的文件队列一般为空。
func (a *App) flushPending() {
	a.logFile.writeBatch(a.logs.take())
	a.flushToUI()
	a.flushStats()
}

// initLogFile 打开当天的日志文件；失败时只写界面日志，不影响启动。
func (a *App) initLogFile() {
	if a.logsDir == "" {
		return
	}
	lf, err := openLogFile(a.logsDir, time.Now())
	if err != nil {
		a.appendLog("日志文件打开失败（继续只显示在界面）："+err.Error(), 1)
		return
	}
	a.logFile = lf
}

// drainLogsToFile 只把待写日志落盘，不触碰任何 UI 控件（退出时用）。
func (a *App) drainLogsToFile() {
	a.logFile.writeBatch(a.logs.take())
}

func (a *App) scrollLogToEnd() {
	if a.teLog == nil {
		return
	}
	n := a.teLog.TextLength()
	a.teLog.SetTextSelection(n, n)
	a.teLog.ScrollToCaret()
}

// appendLog 只入队，不做任何 UI 调用，因此可从 worker goroutine 安全调用。
func (a *App) appendLog(msg string, level int) {
	prefix := "· "
	if level == 1 {
		prefix = "! "
	} else if level == 2 {
		prefix = "x "
	}
	line := time.Now().Format("15:04:05") + "  " + prefix + msg

	a.logMu.Lock()
	closed := a.closed
	a.logMu.Unlock()
	if closed {
		return
	}
	a.logs.push(line)
}

func (a *App) markClosed() {
	a.logMu.Lock()
	a.closed = true
	a.logMu.Unlock()
}

func (a *App) safeSync(fn func()) {
	defer func() { _ = recover() }()
	if a.mw == nil {
		return
	}
	a.logMu.Lock()
	closed := a.closed
	a.logMu.Unlock()
	if closed {
		return
	}
	a.mw.Synchronize(fn)
}

// ---------- 回调（来自 worker goroutine） ----------

// onWorkerLog 不再节流：每张图片、每条告警都会进入日志。
func (a *App) onWorkerLog(level int, msg string) {
	a.appendLog(msg, level)
}

// onWorkerStat 只记录快照，不投递 UI 消息；界面更新统一由 100ms 泵完成。
func (a *App) onWorkerStat(s Stats) {
	a.statMu.Lock()
	a.lastStats, a.statDirty = s, true
	a.statMu.Unlock()
}

// flushStats 必须在 UI 线程调用：把最新统计一次性写到状态栏，并在任务
// 自动结束时恢复「开始」按钮。
func (a *App) flushStats() {
	a.statMu.Lock()
	dirty, s := a.statDirty, a.lastStats
	a.statDirty = false
	a.statMu.Unlock()
	if !dirty || a.lblStat == nil {
		return
	}
	text := fmt.Sprintf("已生成 %d 张 | 失败 %d | 合计 %.1f MB | 单张 %dms",
		s.Gen, s.Fail, float64(s.Bytes)/1048576.0, s.LastDur.Milliseconds())
	if a.limitCount > 0 {
		text += fmt.Sprintf(" | 上限 %d 张", a.limitCount)
	}
	_ = a.lblStat.SetText(text)
	if a.worker != nil && !a.worker.Running() {
		a.setRunning(false)
	}
}

// ---------- 自检模式 ----------

func (a *App) runOnce() {
	cfg := a.cfg
	dir := cfg.OutDir
	if cfg.DateSubdir {
		dir = filepath.Join(dir, time.Now().Format("20060102"))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "创建目录失败:", err)
		return
	}
	res, err := Build(Spec{
		Width: cfg.Width, Height: cfg.Height,
		TargetBytes: int64(cfg.TargetKB) * 1024,
		Format:      cfg.Format, Pad: cfg.Pad,
		Quality: cfg.Quality, AutoQuality: cfg.AutoQuality,
		Seed: time.Now().UnixNano(), Style: Style(cfg.Style),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "生成失败:", err)
		return
	}
	// 自检只有一张：轮数 001、本轮序号 000001，与 GUI 命名格式保持一致。
	path := filepath.Join(dir, fmt.Sprintf("%s_%s_%03d_%06d.%s",
		sanitizePrefix(cfg.Prefix), time.Now().Format("20060102_150405.000"), 1, 1, extOf(cfg.Format)))
	if err := os.WriteFile(path, res.Data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写入失败:", err)
		return
	}
	fmt.Printf("OK %s size=%d target=%d quality=%d note=%s style=%s\n",
		path, len(res.Data), cfg.TargetKB*1024, res.Quality, res.Note, cfg.Style)
}
