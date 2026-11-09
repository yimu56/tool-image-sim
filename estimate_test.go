package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

// 界面/配置默认质量为 100；Spec 未指定质量时的内部兜底仍为 88，
// 两者刻意分离，避免改变既有调用的输出与 golden 基线。
func TestDefaultConfigQualityIs100(t *testing.T) {
	dir := t.TempDir()
	if got := defaultConfig(dir).Quality; got != 100 {
		t.Fatalf("界面默认质量应为 100，实际 %d", got)
	}
	if defaultConfigQuality != 100 {
		t.Fatalf("defaultConfigQuality 应为 100，实际 %d", defaultConfigQuality)
	}
	if effectiveQuality(0) != 88 || defaultJPEGQuality != 88 {
		t.Fatal("Spec 内部质量兜底必须保持 88，否则会改变未指定质量时的产物")
	}
}

func estimateSpecOf(cfg Config) EstimateSpec {
	return EstimateSpec{
		Width: cfg.Width, Height: cfg.Height,
		Format: cfg.Format, Quality: cfg.Quality, Style: Style(cfg.Style),
		TargetKB: cfg.TargetKB, Pad: cfg.Pad, AutoQuality: cfg.AutoQuality,
	}
}

// 单值预测必须与真实产物一致：文件字节数完全相等，图像数据体积接近。
func TestEstimateDefaultConfigMatchesRealBuild(t *testing.T) {
	cfg := defaultConfig(t.TempDir())
	est, err := EstimateSize(estimateSpecOf(cfg))
	if err != nil {
		t.Fatal(err)
	}
	real, err := Build(Spec{
		Width: cfg.Width, Height: cfg.Height,
		TargetBytes: int64(cfg.TargetKB) * 1024,
		Format:      cfg.Format, Pad: cfg.Pad,
		Quality: cfg.Quality, AutoQuality: cfg.AutoQuality,
		Seed: 20261109,
	})
	if err != nil {
		t.Fatal(err)
	}
	decodeAny(t, real.Data, cfg.Format)

	if est.FileBytes != int64(len(real.Data)) {
		t.Fatalf("预计文件字节数应等于真实产物：预计 %d，实际 %d", est.FileBytes, len(real.Data))
	}
	diff := math.Abs(float64(est.ImageBytes)-float64(real.Encoded)) / float64(real.Encoded)
	if diff > 0.25 {
		t.Fatalf("预计图像数据体积偏差过大：预计 %d，实际 %d（%.1f%%）",
			est.ImageBytes, real.Encoded, diff*100)
	}
	t.Logf("默认配置 %dx%d %s 质量 %d：%s",
		cfg.Width, cfg.Height, cfg.Format, cfg.Quality, est.Display())
	t.Logf("真实产物：文件 %d 字节，图像数据 %d 字节，细节 %.2f；图像数据预测偏差 %.1f%%",
		len(real.Data), real.Encoded, real.Detail, diff*100)
}

// 估算必须足够快，才能放在 UI 线程同步执行、做到输入即变。
func TestEstimateIsFastEnoughForUIThread(t *testing.T) {
	spec := estimateSpecOf(defaultConfig(t.TempDir()))
	start := time.Now()
	if _, err := EstimateSize(spec); err != nil {
		t.Fatal(err)
	}
	d := time.Since(start)
	if d > 200*time.Millisecond {
		t.Fatalf("单次估算耗时 %v，会阻塞 UI 线程", d)
	}
	t.Logf("默认配置单次估算耗时 %v", d)
}

func TestEstimateSingleValueAndPadding(t *testing.T) {
	spec := EstimateSpec{
		Width: 1280, Height: 960, Format: FmtJPG, Quality: 100,
		TargetKB: 800, Pad: true, AutoQuality: true,
	}
	est, err := EstimateSize(spec)
	if err != nil {
		t.Fatal(err)
	}
	if est.FileBytes != 800*1024 {
		t.Fatalf("严格填充时预计文件应为 819200 字节，实际 %d", est.FileBytes)
	}
	if est.ImageBytes <= 0 || est.ImageBytes >= est.FileBytes || est.PaddedBytes != est.FileBytes-est.ImageBytes {
		t.Fatalf("图像数据/填充量不合法：%+v", est)
	}
	if est.BelowMinimum || strings.Contains(est.Display(), "~") {
		t.Fatalf("不应再出现区间或下限告警：%+v / %s", est, est.Display())
	}

	spec.Pad = false
	noPad, err := EstimateSize(spec)
	if err != nil {
		t.Fatal(err)
	}
	if noPad.FileBytes != noPad.ImageBytes || noPad.PaddedBytes != 0 {
		t.Fatalf("未填充时预计文件应等于图像数据：%+v", noPad)
	}
}

func TestEstimateBelowMinimum(t *testing.T) {
	est, err := EstimateSize(EstimateSpec{
		Width: 1280, Height: 960, Format: FmtPNG, Quality: 100,
		TargetKB: 1, Pad: true, AutoQuality: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !est.BelowMinimum || est.FileBytes != est.ImageBytes || est.FileBytes <= 1024 {
		t.Fatalf("目标低于可编码下限时应明确告警：%+v", est)
	}
	if !strings.Contains(est.Display(), "下限") {
		t.Fatalf("显示文本应包含下限告警：%s", est.Display())
	}
}

func TestEstimateSizeBMPFixed(t *testing.T) {
	rowSize := (200*3 + 3) &^ 3
	want := int64(54 + rowSize*100)
	est, err := EstimateSize(EstimateSpec{Width: 200, Height: 100, Format: FmtBMP, Quality: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !est.BMPFixed || est.ImageBytes != want || est.FileBytes != want || est.PaddedBytes != 0 {
		t.Fatalf("BMP 体积应由尺寸锁定为 %d，实际 %+v", want, est)
	}
	res, err := Build(Spec{Width: 200, Height: 100, Format: FmtBMP, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(res.Data)) != want {
		t.Fatalf("估算应与真实 BMP 产物一致：%d vs %d", want, len(res.Data))
	}
}

// 分辨率、质量、格式变化时，单值预测必须跟着变（这是「实时变化」的核心）。
func TestEstimateReflectsQualityFormatAndDimensions(t *testing.T) {
	base := EstimateSpec{
		Width: 320, Height: 240, Format: FmtJPG, Quality: 20,
		TargetKB: 204800, Pad: false, AutoQuality: false,
	}
	q20, err := EstimateSize(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Quality = 100
	q100, err := EstimateSize(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Format = FmtPNG
	png, err := EstimateSize(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Width, base.Height = 640, 480
	big, err := EstimateSize(base)
	if err != nil {
		t.Fatal(err)
	}

	if q100.ImageBytes <= q20.ImageBytes {
		t.Fatalf("质量 100 应比质量 20 更大：%d vs %d", q100.ImageBytes, q20.ImageBytes)
	}
	if png.ImageBytes <= q100.ImageBytes {
		t.Fatalf("噪声图 PNG 通常比 JPG 更大：%d vs %d", png.ImageBytes, q100.ImageBytes)
	}
	if big.ImageBytes <= png.ImageBytes {
		t.Fatalf("更大分辨率应更大：%d vs %d", big.ImageBytes, png.ImageBytes)
	}
	t.Logf("q20=%d B / q100=%d B / PNG=%d B / 640x480 PNG=%d B（图像数据）",
		q20.ImageBytes, q100.ImageBytes, png.ImageBytes, big.ImageBytes)
}

func TestEstimateReflectsTargetAndPad(t *testing.T) {
	spec := EstimateSpec{
		Width: 640, Height: 480, Format: FmtJPG, Quality: 88,
		TargetKB: 200, Pad: true, AutoQuality: true,
	}
	a, err := EstimateSize(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.TargetKB = 1000
	b, err := EstimateSize(spec)
	if err != nil {
		t.Fatal(err)
	}
	if b.FileBytes != 1000*1024 || a.FileBytes != 200*1024 || b.PaddedBytes <= a.PaddedBytes {
		t.Fatalf("目标大小变化必须反映到预计文件：%+v / %+v", a, b)
	}
}

func TestEstimateSizeMinecraftIsMoreCompressible(t *testing.T) {
	base := EstimateSpec{
		Width: 320, Height: 240, Format: FmtPNG, Quality: 100,
		TargetKB: 204800, Pad: false,
	}
	orig, err := EstimateSize(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Style = StyleMinecraft
	mc, err := EstimateSize(base)
	if err != nil {
		t.Fatal(err)
	}
	if mc.ImageBytes >= orig.ImageBytes {
		t.Fatalf("方块化后应更易于压缩：minecraft=%d original=%d", mc.ImageBytes, orig.ImageBytes)
	}
	t.Logf("original=%s / minecraft=%s", orig.Display(), mc.Display())
}

func TestEstimateSizeInvalidAndSampleScale(t *testing.T) {
	if _, err := EstimateSize(EstimateSpec{Width: 1, Height: 100, Format: FmtJPG}); err == nil {
		t.Fatal("非法尺寸应报错")
	}
	if _, err := EstimateSize(EstimateSpec{Width: 320, Height: 240, TargetKB: -1}); err == nil {
		t.Fatal("非法目标应报错")
	}
	if sw, sh := estimateSampleSize(320, 240, estimateSamplePixels); sw != 320 || sh != 240 {
		t.Fatalf("小图不应被缩放：%dx%d", sw, sh)
	}
	sw, sh := estimateSampleSize(16000, 16000, estimateSamplePixels)
	if sw >= 16000 || sh >= 16000 || sw < 8 || sh < 8 {
		t.Fatalf("超大尺寸应等比缩小：%dx%d", sw, sh)
	}

	// 用极小的采样上限覆盖面积还原路径，避免测试真跑大图。
	est, err := estimateSize(EstimateSpec{
		Width: 1280, Height: 960, Format: FmtJPG, Quality: 88,
		TargetKB: 800, Pad: true,
	}, 64*64)
	if err != nil {
		t.Fatal(err)
	}
	if !est.Extrapolated || est.ImageBytes <= 0 || est.FileBytes <= 0 {
		t.Fatalf("面积还原结果非法：%+v", est)
	}
}

func TestSizeEstimateDisplay(t *testing.T) {
	e := SizeEstimate{ImageBytes: 700 * 1024, FileBytes: 800 * 1024, PaddedBytes: 100 * 1024}
	s := e.Display()
	if !strings.Contains(s, "预计文件") || !strings.Contains(s, "填充") || strings.Contains(s, "~") {
		t.Fatalf("显示文本应是单值且说明填充：%s", s)
	}
	b := SizeEstimate{ImageBytes: 60054, FileBytes: 60054, BMPFixed: true}
	if !strings.Contains(b.Display(), "BMP") {
		t.Fatalf("BMP 应注明体积由尺寸锁定：%s", b.Display())
	}
}

// 估算使用独立固定种子，不得扰动真实生成链路的任何状态。
func TestEstimateDoesNotAffectBuild(t *testing.T) {
	s := Spec{Width: 160, Height: 120, TargetBytes: 20 * 1024, Format: FmtJPG, Pad: true, Seed: 4242}
	before, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EstimateSize(EstimateSpec{
		Width: 160, Height: 120, Format: FmtPNG, Quality: 100,
		TargetKB: 100, Pad: true, Style: StyleMinecraft,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Data, after.Data) {
		t.Fatal("估算前后同一种子的产物必须完全一致")
	}
}
