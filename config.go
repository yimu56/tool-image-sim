package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// defaultConfigQuality 是界面与 config.json 的默认 JPEG 质量。
// 注意与 generator.go 的 defaultJPEGQuality 区分：后者只在 Spec 未指定
// Quality 时作为内部兜底，保持不变以免影响既有生成结果与回归基线。
const defaultConfigQuality = 100

// 运行限制的合法上限：7 天 / 10 亿张。上限同时用于界面校验与配置兜底，
// 避免手改 config.json 时 time.Duration 溢出。
const (
	maxLimitMinutes = 7 * 24 * 60
	maxLimitCount   = int64(1_000_000_000)
)

// Config 是持久化到 config.json 的界面参数。
type Config struct {
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	TargetKB    int    `json:"target_kb"`
	Format      string `json:"format"`
	Pad         bool   `json:"pad_to_target"`
	Quality     int    `json:"quality"`
	AutoQuality bool   `json:"auto_quality"`
	IntervalMS  int    `json:"interval_ms"`
	Prefix      string `json:"prefix"`
	OutDir      string `json:"out_dir"`
	DateSubdir  bool   `json:"date_subdir"`
	Style       string `json:"style"` // original（默认）| minecraft

	// 运行限制：0 表示不限；两者都非 0 时满足任一即自动停止。
	LimitMinutes int   `json:"limit_minutes"`
	LimitCount   int64 `json:"limit_count"`

	Comment string `json:"_说明,omitempty"`
}

func defaultConfig(exeDir string) Config {
	return Config{
		Width:        1280,
		Height:       960,
		TargetKB:     800,
		Format:       FmtJPG,
		Pad:          true,
		Quality:      defaultConfigQuality,
		AutoQuality:  true,
		IntervalMS:   1000,
		Prefix:       "IMG",
		OutDir:       filepath.Join(exeDir, "output"),
		DateSubdir:   true,
		Style:        string(StyleOriginal),
		LimitMinutes: 0,
		LimitCount:   0,
		Comment:      "style=图片风格(original|minecraft, 默认 original); quality=JPEG质量(1-100); auto_quality=压不到目标时自动降质量; pad_to_target=true 时用图片元数据填充到精确字节数; interval_ms=生成间隔毫秒; target_kb=目标文件大小(KB); limit_minutes=运行时长上限(分钟, 0=不限); limit_count=生成张数上限(0=不限)",
	}
}

// loadConfig 读取配置；文件不存在或损坏时按默认值返回（不覆盖用户文件）。
func loadConfig(path, exeDir string) Config {
	cfg := defaultConfig(exeDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return cfg
	}
	// 逐项兜底，避免配置文件缺字段时出现 0 值
	if c.Width <= 0 {
		c.Width = cfg.Width
	}
	if c.Height <= 0 {
		c.Height = cfg.Height
	}
	if c.TargetKB <= 0 {
		c.TargetKB = cfg.TargetKB
	}
	if c.IntervalMS <= 0 {
		c.IntervalMS = cfg.IntervalMS
	}
	if c.Quality <= 0 {
		c.Quality = cfg.Quality
	}
	// 旧版本配置没有 auto_quality 字段，bool 零值会被误读成 false；
	// 只有字段确实存在时才尊重用户显式设置的 false。
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) == nil {
		if _, ok := probe["auto_quality"]; !ok {
			c.AutoQuality = cfg.AutoQuality
		}
	}
	if c.Format == "" {
		c.Format = cfg.Format
	}
	// 旧版本配置没有 style 字段：零值按默认 original 处理。
	// 非法值回退 original，并通过 styleWarnf 输出清晰日志（启动阶段注入）。
	c.Style = string(resolveStyle(c.Style))
	if c.Prefix == "" {
		c.Prefix = cfg.Prefix
	}
	if c.OutDir == "" {
		c.OutDir = cfg.OutDir
	}
	// 运行限制：缺字段的零值即「不限」；负数与超范围值收敛到合法区间，
	// 避免手改配置文件时 time.Duration 溢出或出现无意义的上限。
	if c.LimitMinutes < 0 {
		c.LimitMinutes = 0
	}
	if c.LimitMinutes > maxLimitMinutes {
		c.LimitMinutes = maxLimitMinutes
	}
	if c.LimitCount < 0 {
		c.LimitCount = 0
	}
	if c.LimitCount > maxLimitCount {
		c.LimitCount = maxLimitCount
	}
	return c
}

func saveConfig(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
