package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- 枚举、默认值、非法值回退 ----------

func TestParseStyleDefaultsAndInvalid(t *testing.T) {
	cases := []struct {
		in   string
		want Style
		ok   bool
	}{
		{"", StyleOriginal, true},
		{"   ", StyleOriginal, true},
		{"original", StyleOriginal, true},
		{" Original ", StyleOriginal, true},
		{"minecraft", StyleMinecraft, true},
		{"  MINECRAFT\t", StyleMinecraft, true},
		{"pixel", StyleOriginal, false},
		{" mc ", StyleOriginal, false},
	}
	for _, c := range cases {
		got, ok := parseStyle(c.in)
		if got != c.want || ok != c.ok {
			t.Fatalf("parseStyle(%q)=(%q,%v)，期望 (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}

	if names := StyleNames(); len(names) < 2 || names[0] != string(StyleOriginal) || names[1] != string(StyleMinecraft) {
		t.Fatalf("风格列表必须以 original 打头且包含 minecraft：%v", names)
	}
	if transformFor(StyleOriginal) != nil {
		t.Fatal("original 不得注册任何后处理函数")
	}
	if transformFor(StyleMinecraft) == nil {
		t.Fatal("minecraft 必须注册后处理函数")
	}
	if indexOfStyle(StyleMinecraft) != 1 || indexOfStyle(Style("unknown")) != 0 {
		t.Fatal("下拉框索引映射错误")
	}
}

func captureStyleWarnings(t *testing.T) *[]string {
	t.Helper()
	old := styleWarnf
	msgs := &[]string{}
	styleWarnf = func(msg string) { *msgs = append(*msgs, msg) }
	t.Cleanup(func() { styleWarnf = old })
	return msgs
}

func TestResolveStyleInvalidWarns(t *testing.T) {
	msgs := captureStyleWarnings(t)
	if got := resolveStyle("vangogh"); got != StyleOriginal {
		t.Fatalf("非法值应回退 original，实际 %q", got)
	}
	if len(*msgs) != 1 || !strings.Contains((*msgs)[0], "vangogh") || !strings.Contains((*msgs)[0], "original") {
		t.Fatalf("非法值必须输出清晰告警，实际 %v", *msgs)
	}
	if got := resolveStyle("minecraft"); got != StyleMinecraft {
		t.Fatalf("合法值应正常解析，实际 %q", got)
	}
	if got := resolveStyle(""); got != StyleOriginal {
		t.Fatalf("空值应为默认 original，实际 %q", got)
	}
	if len(*msgs) != 1 {
		t.Fatalf("合法值/空值不应产生告警，实际 %v", *msgs)
	}
}

// ---------- 配置解析与持久化 ----------

func TestConfigStyleDefaultPersistAndFallback(t *testing.T) {
	dir := t.TempDir()
	exeDir := filepath.Join(dir, "app")
	path := filepath.Join(dir, "config.json")

	if got := loadConfig(path, exeDir); got.Style != string(StyleOriginal) {
		t.Fatalf("无配置文件时风格应为 original，实际 %q", got.Style)
	}
	if got := defaultConfig(exeDir); got.Style != string(StyleOriginal) {
		t.Fatalf("默认配置风格应为 original，实际 %q", got.Style)
	}

	cfg := defaultConfig(exeDir)
	cfg.Style = string(StyleMinecraft)
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"style": "minecraft"`) {
		t.Fatalf("风格必须写入 config.json：%s", raw)
	}
	if got := loadConfig(path, exeDir); got.Style != string(StyleMinecraft) {
		t.Fatalf("风格应能从 config.json 读回，实际 %q", got.Style)
	}

	// 旧版本配置（没有 style 字段）→ 默认 original，且不告警。
	if err := os.WriteFile(path, []byte(`{"width":640,"height":480}`), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs := captureStyleWarnings(t)
	if got := loadConfig(path, exeDir); got.Style != string(StyleOriginal) {
		t.Fatalf("旧配置缺失 style 应回退 original，实际 %q", got.Style)
	}
	if len(*msgs) != 0 {
		t.Fatalf("缺失字段不应告警，实际 %v", *msgs)
	}

	// 非法值 → 回退 original + 清晰日志。
	if err := os.WriteFile(path, []byte(`{"width":640,"height":480,"style":"pixel"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig(path, exeDir); got.Style != string(StyleOriginal) {
		t.Fatalf("非法配置值应回退 original，实际 %q", got.Style)
	}
	if len(*msgs) != 1 || !strings.Contains((*msgs)[0], "pixel") {
		t.Fatalf("非法配置值必须记录日志，实际 %v", *msgs)
	}
}

// ---------- 转换结果：尺寸、体积、有限调色板、明暗结构、确定性 ----------

func bmpDimensions(t *testing.T, d []byte) (int, int) {
	t.Helper()
	if len(d) < 30 || d[0] != 'B' || d[1] != 'M' {
		t.Fatal("BMP 头部非法")
	}
	return int(int32(binary.LittleEndian.Uint32(d[18:22]))), int(int32(binary.LittleEndian.Uint32(d[22:26])))
}

func TestMinecraftBuildSizeAndDimensions(t *testing.T) {
	cases := []struct {
		name   string
		spec   Spec
		target int64
	}{
		{"jpg_pad", Spec{Width: 320, Height: 240, Format: FmtJPG, Pad: true, Quality: 88, AutoQuality: true}, 60 * 1024},
		{"png_pad", Spec{Width: 320, Height: 240, Format: FmtPNG, Pad: true}, 120 * 1024},
		{"png_pad_odd_size", Spec{Width: 333, Height: 217, Format: FmtPNG, Pad: true}, 50 * 1024},
		{"jpg_nopad", Spec{Width: 400, Height: 300, Format: FmtJPG, Pad: false}, 80 * 1024},
		{"bmp", Spec{Width: 200, Height: 100, Format: FmtBMP, Pad: false}, 30 * 1024},
		{"no_target", Spec{Width: 160, Height: 120, Format: FmtJPG, Pad: true}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.spec.TargetBytes = c.target
			c.spec.Seed = 12345
			c.spec.Style = StyleMinecraft
			before := minecraftTransformCalls.Load()

			res, err := Build(c.spec)
			if err != nil {
				t.Fatalf("Build 失败: %v", err)
			}
			if res.Style != StyleMinecraft || !res.Transformed {
				t.Fatalf("应标记为已转换风格：style=%q transformed=%v", res.Style, res.Transformed)
			}
			if minecraftTransformCalls.Load() <= before {
				t.Fatal("minecraft 风格应执行本地转换")
			}

			if c.target > 0 && c.spec.Pad && c.spec.Format != FmtBMP {
				if int64(len(res.Data)) != c.target {
					t.Fatalf("应精确命中目标 %d 字节，实际 %d（note=%q）", c.target, len(res.Data), res.Note)
				}
			}
			if c.target > 0 && !c.spec.Pad && c.spec.Format != FmtBMP && int64(len(res.Data)) > c.target {
				t.Fatalf("未填充时不应超过目标：%d > %d", len(res.Data), c.target)
			}

			if c.spec.Format == FmtBMP {
				w, h := bmpDimensions(t, res.Data)
				if w != c.spec.Width || h != c.spec.Height {
					t.Fatalf("BMP 尺寸不符：%dx%d", w, h)
				}
			} else {
				img := decodeAny(t, res.Data, c.spec.Format)
				if b := img.Bounds(); b.Dx() != c.spec.Width || b.Dy() != c.spec.Height {
					t.Fatalf("尺寸不符：%v", b)
				}
			}

			if res.Padded && !strings.Contains(res.Note, "风格 minecraft") {
				t.Fatalf("填充后必须说明风格对体积的影响，note=%q", res.Note)
			}
			if !c.spec.Pad && c.target > 0 && int64(len(res.Data)) < c.target &&
				c.spec.Format != FmtBMP && !strings.Contains(res.Note, "未做填充") {
				t.Fatalf("未填充时必须说明缺口，note=%q", res.Note)
			}
		})
	}
}

func TestMinecraftTransformKeepsSizeAndLimitedPalette(t *testing.T) {
	src := renderRGBA(256, 192, 0.8, 20261109)
	out := convertMinecraft(src)
	if got, want := out.Bounds(), src.Bounds(); got != want {
		t.Fatalf("转换必须保持尺寸：%v -> %v", want, got)
	}

	allowed := map[uint32]bool{}
	for l := 0; l < mcLevels; l++ {
		for f := 0; f < mcFamilies; f++ {
			for s := 0; s < mcShades; s++ {
				c := minecraftShades[l][f][s]
				allowed[uint32(c.r)<<16|uint32(c.g)<<8|uint32(c.b)] = true
			}
		}
	}
	seen := map[uint32]bool{}
	for i := 0; i < len(out.Pix); i += 4 {
		if out.Pix[i+3] != 255 {
			t.Fatalf("alpha 必须恒为 255，位置 %d 为 %d", i, out.Pix[i+3])
		}
		key := uint32(out.Pix[i])<<16 | uint32(out.Pix[i+1])<<8 | uint32(out.Pix[i+2])
		if !allowed[key] {
			t.Fatalf("出现色板之外的颜色 #%06x", key)
		}
		seen[key] = true
	}
	if len(seen) < 6 || len(seen) > len(allowed) {
		t.Fatalf("应为有限调色板且确实发生量化：实际 %d 色，色板 %d 色", len(seen), len(allowed))
	}
	t.Logf("转换后实际使用 %d 种颜色（有限色板上限 %d）", len(seen), len(allowed))
	if bytes.Equal(src.Pix, out.Pix) {
		t.Fatal("转换结果不应与原图逐像素相同")
	}
}

// blockLumas 取每个方块单元的平均亮度，用于核对明暗结构是否被保留。
func blockLumas(img *image.RGBA) []float64 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	base := img.PixOffset(b.Min.X, b.Min.Y)
	const bs = minecraftBlockSize
	out := make([]float64, 0, ((w+bs-1)/bs)*((h+bs-1)/bs))
	for by := 0; by < h; by += bs {
		bh := min(bs, h-by)
		for bx := 0; bx < w; bx += bs {
			bw := min(bs, w-bx)
			var sum int
			for ly := 0; ly < bh; ly++ {
				i := base + (by+ly)*img.Stride + bx*4
				for lx := 0; lx < bw; lx++ {
					sum += (int(img.Pix[i])*77 + int(img.Pix[i+1])*150 + int(img.Pix[i+2])*29) >> 8
					i += 4
				}
			}
			out = append(out, float64(sum)/float64(bw*bh))
		}
	}
	return out
}

func pearson(xs, ys []float64) float64 {
	var sx, sy float64
	n := float64(len(xs))
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
	}
	mx, my := sx/n, sy/n
	var sxx, syy, sxy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		sxx += dx * dx
		syy += dy * dy
		sxy += dx * dy
	}
	if sxx <= 0 || syy <= 0 {
		return 0
	}
	return sxy / math.Sqrt(sxx*syy)
}

func TestMinecraftPreservesLuminanceStructure(t *testing.T) {
	src := renderRGBA(320, 240, 0.9, 987654321)
	out := convertMinecraft(src)
	a, b := blockLumas(src), blockLumas(out)
	if len(a) != len(b) {
		t.Fatalf("方块数不一致：%d vs %d", len(a), len(b))
	}
	r := pearson(a, b)
	t.Logf("原图 / 风格图块平均亮度 Pearson 相关系数 = %.4f（%d 个方块）", r, len(a))
	if r < 0.8 {
		t.Fatalf("转换后块亮度与原图相关性过低（%.3f）：明暗结构被破坏", r)
	}
}

func TestMinecraftTransformDeterministic(t *testing.T) {
	a := convertMinecraft(renderRGBA(128, 96, 0.7, 42))
	b := convertMinecraft(renderRGBA(128, 96, 0.7, 42))
	c := convertMinecraft(renderRGBA(128, 96, 0.7, 43))
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Fatal("相同原始图必须得到完全相同的转换结果")
	}
	if bytes.Equal(a.Pix, c.Pix) {
		t.Fatal("不同种子的原始图应得到不同的转换结果")
	}
}

// ---------- original 路径隔离 ----------

func TestOriginalPathNeverTransforms(t *testing.T) {
	msgs := captureStyleWarnings(t)
	before := minecraftTransformCalls.Load()

	for _, s := range []Style{"", StyleOriginal, Style("not-a-style")} {
		res, err := Build(Spec{
			Width: 320, Height: 240, TargetBytes: 60 * 1024,
			Format: FmtJPG, Pad: true, Seed: 12345, Style: s,
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Style != StyleOriginal || res.Transformed {
			t.Fatalf("Style=%q 应始终按 original 处理：style=%q transformed=%v", s, res.Style, res.Transformed)
		}
		if strings.Contains(res.Note, "风格") {
			t.Fatalf("original 不应附加风格说明：note=%q", res.Note)
		}
	}
	if minecraftTransformCalls.Load() != before {
		t.Fatal("original 路径不得调用 Minecraft 转换函数")
	}
	if len(*msgs) != 1 || !strings.Contains((*msgs)[0], "not-a-style") {
		t.Fatalf("非法 Spec.Style 也应记录回退日志，实际 %v", *msgs)
	}

	if _, err := Build(Spec{
		Width: 320, Height: 240, TargetBytes: 60 * 1024,
		Format: FmtJPG, Pad: true, Seed: 12345, Style: StyleMinecraft,
	}); err != nil {
		t.Fatal(err)
	}
	if minecraftTransformCalls.Load() <= before {
		t.Fatal("切换到 minecraft 后应执行转换")
	}
}

// -style / -autostart 的临时覆盖绝不能被 saveConfig 写回默认配置，
// 否则试跑一次 minecraft 后，下次启动的「默认风格」就变了。
func TestStyleOverrideIsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	base := defaultConfig(dir)
	if err := saveConfig(path, base); err != nil {
		t.Fatal(err)
	}

	disk := loadConfig(path, dir)
	if disk.Style != string(StyleOriginal) {
		t.Fatalf("默认配置应为 original，实际 %q", disk.Style)
	}
	override := string(resolveStyle("minecraft"))
	ui := disk
	ui.Style = override // 界面/本次运行看到的是被覆盖后的风格
	if ui.Style != string(StyleMinecraft) {
		t.Fatal("本次运行应使用 minecraft")
	}

	saved := configForPersist(ui, override, disk.Style)
	if saved.Style != string(StyleOriginal) {
		t.Fatalf("临时覆盖不应写入配置文件，实际 %q", saved.Style)
	}
	if err := saveConfig(path, saved); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig(path, dir); got.Style != string(StyleOriginal) {
		t.Fatalf("重新读取配置后默认风格应仍是 original，实际 %q", got.Style)
	}

	// 没有 -style 时，GUI 里主动选择仍应正常持久化（原有机制）。
	ui.Style = string(StyleMinecraft)
	if got := configForPersist(ui, "", disk.Style); got.Style != string(StyleMinecraft) {
		t.Fatalf("界面主动选择应被保存，实际 %q", got.Style)
	}
}

// worker -> Spec 的实际链路：风格能被透传、落盘文件尺寸/体积正确、日志有标记。
func TestWorkerPassesStyleToBuild(t *testing.T) {
	dir := t.TempDir()
	var logs []string
	w := NewWorker(func(level int, msg string) { logs = append(logs, msg) }, nil)
	cfg := RunConfig{
		Width: 128, Height: 96, TargetBytes: 20 * 1024,
		Format: FmtPNG, Pad: true, Interval: minInterval,
		Prefix: "STYLE", OutDir: dir, Style: StyleMinecraft,
	}

	w.generate(cfg)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("应产出 1 张图片：%v, %v", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) != cfg.TargetBytes {
		t.Fatalf("文件大小应为 %d，实际 %d", cfg.TargetBytes, len(raw))
	}
	if b := decodeAny(t, raw, FmtPNG).Bounds(); b.Dx() != cfg.Width || b.Dy() != cfg.Height {
		t.Fatalf("尺寸不符：%v", b)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "风格 minecraft") {
		t.Fatalf("minecraft 帧日志应带风格标记：%v", logs)
	}

	// 原始风格：日志不追加风格标记（保持原有格式）。
	logs = nil
	cfg.Style = StyleOriginal
	w.generate(cfg)
	if strings.Contains(strings.Join(logs, "\n"), "风格 ") {
		t.Fatalf("original 帧日志不应带风格标记：%v", logs)
	}
}
