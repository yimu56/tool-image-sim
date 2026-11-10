package main

import (
	"strings"
	"testing"
)

func naturalSpec(w, h, targetKB int) EstimateSpec {
	return EstimateSpec{
		Width: w, Height: h, Format: FmtJPG, Quality: 100,
		TargetKB: targetKB, Pad: true, AutoQuality: true,
	}
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// 复现问题场景：目标 2048KB 时，1920×1080 与 1920×3000 的「图像数据」都会
// 被自动降档压到 ≈1.9MB；满细节自然体积必须明显随分辨率增长。
func TestNaturalBytesGrowsWithResolution(t *testing.T) {
	fhd, err := EstimateSize(naturalSpec(1920, 1080, 2048))
	if err != nil {
		t.Fatal(err)
	}
	tall, err := EstimateSize(naturalSpec(1920, 3000, 2048))
	if err != nil {
		t.Fatal(err)
	}
	if tall.NaturalBytes <= fhd.NaturalBytes*2 {
		t.Fatalf("1920×3000 的满细节自然体积应至少是 1920×1080 的两倍：%d vs %d",
			tall.NaturalBytes, fhd.NaturalBytes)
	}
	diff := float64(absInt64(fhd.ImageBytes-tall.ImageBytes)) / float64(fhd.ImageBytes)
	if diff > 0.20 {
		t.Fatalf("目标钳制下图像数据应几乎相同，实际偏差 %.1f%%：%d vs %d",
			diff*100, fhd.ImageBytes, tall.ImageBytes)
	}
	if fhd.FileBytes != 2048*1024 || tall.FileBytes != 2048*1024 {
		t.Fatal("严格填充下预计文件仍应等于目标大小")
	}
	t.Logf("目标 2048KB：1920×1080 图像数据 %s / 自然体积 %s；1920×3000 图像数据 %s / 自然体积 %s",
		formatSize(fhd.ImageBytes), formatSize(fhd.NaturalBytes),
		formatSize(tall.ImageBytes), formatSize(tall.NaturalBytes))
}

// 满细节自然体积只由尺寸/格式/质量/风格决定，不受目标大小影响。
func TestNaturalBytesIndependentOfTarget(t *testing.T) {
	a, err := EstimateSize(naturalSpec(1920, 1080, 2048))
	if err != nil {
		t.Fatal(err)
	}
	b, err := EstimateSize(naturalSpec(1920, 1080, 8192))
	if err != nil {
		t.Fatal(err)
	}
	if a.NaturalBytes != b.NaturalBytes {
		t.Fatalf("满细节自然体积不应随目标大小变化：%d vs %d", a.NaturalBytes, b.NaturalBytes)
	}
}

func TestNaturalBytesReflectsQualityFormatAndStyle(t *testing.T) {
	q20 := naturalSpec(640, 480, 2048)
	q20.Quality = 20
	q100 := naturalSpec(640, 480, 2048)
	png := naturalSpec(640, 480, 2048)
	png.Format = FmtPNG
	mc := naturalSpec(640, 480, 2048)
	mc.Style = StyleMinecraft

	a, err := EstimateSize(q20)
	if err != nil {
		t.Fatal(err)
	}
	b, err := EstimateSize(q100)
	if err != nil {
		t.Fatal(err)
	}
	c, err := EstimateSize(png)
	if err != nil {
		t.Fatal(err)
	}
	d, err := EstimateSize(mc)
	if err != nil {
		t.Fatal(err)
	}
	if b.NaturalBytes <= a.NaturalBytes {
		t.Fatalf("质量 100 的满细节自然体积应更大：%d vs %d", b.NaturalBytes, a.NaturalBytes)
	}
	if c.NaturalBytes <= b.NaturalBytes {
		t.Fatalf("噪声图 PNG 的满细节自然体积应更大：%d vs %d", c.NaturalBytes, b.NaturalBytes)
	}
	if d.NaturalBytes >= b.NaturalBytes {
		t.Fatalf("方块化后满细节自然体积应更小：%d vs %d", d.NaturalBytes, b.NaturalBytes)
	}
}

func TestNaturalDisplayAndBMP(t *testing.T) {
	bmp, err := EstimateSize(EstimateSpec{Width: 200, Height: 100, Format: FmtBMP, Quality: 100})
	if err != nil {
		t.Fatal(err)
	}
	if bmp.NaturalBytes != bmp.FileBytes || !strings.Contains(bmp.NaturalDisplay(), "BMP") {
		t.Fatalf("BMP 的满细节自然体积应由尺寸锁定：%+v / %s", bmp, bmp.NaturalDisplay())
	}

	jpg, err := EstimateSize(naturalSpec(1920, 3000, 2048))
	if err != nil {
		t.Fatal(err)
	}
	s := jpg.NaturalDisplay()
	if !strings.Contains(s, "满细节") || strings.Contains(s, "~") || strings.Contains(s, "区间") {
		t.Fatalf("自然体积文案应说明自动降档且为单值：%s", s)
	}
	// Display 保持原有单值口径不变，不受新增字段影响。
	if !strings.Contains(jpg.Display(), "预计文件") || !strings.Contains(jpg.Display(), "图像数据") {
		t.Fatalf("原有预计文件文案不应被改动：%s", jpg.Display())
	}
}
