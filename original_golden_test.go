package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// 这些指纹在「加入风格功能之前」用同一套固定参数、同一种子采集，
// 是 original 模式的逐字节回归基准。任何对原始路径的意外改动都会在这里失败。
type goldenCase struct {
	name    string
	spec    Spec
	bytes   int
	encoded int
	detail  string
	quality int
	padded  bool
	note    string
	sha256  string
}

var originalGolden = []goldenCase{
	{
		name:  "jpg_pad_60k",
		spec:  Spec{Width: 320, Height: 240, TargetBytes: 60 * 1024, Format: FmtJPG, Pad: true, Quality: 88, AutoQuality: true, Seed: 12345},
		bytes: 61440, encoded: 59621, detail: "1.0000", quality: 88, padded: true, note: "",
		sha256: "a3b0f9987c12b527d7334a52e612875ca8a790cca910adda7b08cedf840bc9e6",
	},
	{
		name:  "png_pad_120k",
		spec:  Spec{Width: 320, Height: 240, TargetBytes: 120 * 1024, Format: FmtPNG, Pad: true, Seed: 12345},
		bytes: 122880, encoded: 109211, detail: "0.0636", quality: 88, padded: true, note: "",
		sha256: "797b24ff96a04e59ba7aca53887c388dc3e2b90daec2319915bc1d202a8926e9",
	},
	{
		name:  "jpg_nopad_80k",
		spec:  Spec{Width: 400, Height: 300, TargetBytes: 80 * 1024, Format: FmtJPG, Pad: false, Seed: 7},
		bytes: 81106, encoded: 81106, detail: "0.6455", quality: 88, padded: false, note: "未填充，比目标小 814 字节",
		sha256: "684b04bc0b7a2261e80e3bc48cbfce526e87f141007abbb108ca268d9546c96f",
	},
	{
		name:  "bmp_200x100",
		spec:  Spec{Width: 200, Height: 100, TargetBytes: 30 * 1024, Format: FmtBMP, Pad: false, Seed: 999},
		bytes: 60054, encoded: 60054, detail: "0.5500", quality: 0, padded: false, note: "BMP 未压缩，大小由尺寸决定（58 KB）",
		sha256: "c7c012c621b80b3f184cc2f0daecb0d86c48d9c3234cfd3c8716d2960dbe8d2d",
	},
	{
		name:  "jpg_small_autoq",
		spec:  Spec{Width: 800, Height: 600, TargetBytes: 30 * 1024, Format: FmtJPG, Pad: true, Quality: 100, AutoQuality: true, Seed: 4242},
		bytes: 30720, encoded: 30359, detail: "0.0000", quality: 85, padded: true, note: "",
		sha256: "6df967c3a501998e6fd34c3fc4d53e65d9a5093a17c1b8b40f5a013619613f20",
	},
	{
		name:  "jpg_no_target",
		spec:  Spec{Width: 160, Height: 120, TargetBytes: 0, Format: FmtJPG, Pad: true, Seed: 555},
		bytes: 12498, encoded: 12498, detail: "0.5000", quality: 88, padded: false, note: "",
		sha256: "ba8e51c05b65fca49c0d89e9d7f342e0655b03ed08a864e28d7f4ab1e56a40f7",
	},
	{
		name:  "jpg_default_big",
		spec:  Spec{Width: 1280, Height: 960, TargetBytes: 800 * 1024, Format: FmtJPG, Pad: true, Quality: 88, AutoQuality: true, Seed: 20261109},
		bytes: 819200, encoded: 739955, detail: "0.5000", quality: 88, padded: true, note: "",
		sha256: "08818fe5c50653c2311cd540a05f77a049077e8e4e5371c87d8e44da11d5f364",
	},
}

func TestOriginalGoldenBytesUnchanged(t *testing.T) {
	for _, c := range originalGolden {
		t.Run(c.name, func(t *testing.T) {
			res, err := Build(c.spec) // 不传 Style：默认 original
			if err != nil {
				t.Fatalf("Build 失败: %v", err)
			}
			checkGolden(t, c, res)
		})
	}
}

func checkGolden(t *testing.T, c goldenCase, res *Result) {
	t.Helper()
	sum := sha256.Sum256(res.Data)
	if got := hex.EncodeToString(sum[:]); got != c.sha256 {
		t.Fatalf("%s 产物与改造前不一致：sha256=%s，期望 %s", c.name, got, c.sha256)
	}
	if len(res.Data) != c.bytes || res.Encoded != c.encoded {
		t.Fatalf("%s 大小不符：data=%d encoded=%d，期望 %d/%d", c.name, len(res.Data), res.Encoded, c.bytes, c.encoded)
	}
	if got := fmt.Sprintf("%.4f", res.Detail); got != c.detail {
		t.Fatalf("%s 细节档位不符：%s，期望 %s", c.name, got, c.detail)
	}
	if res.Quality != c.quality || res.Padded != c.padded || res.Note != c.note {
		t.Fatalf("%s 元数据不符：quality=%d padded=%v note=%q", c.name, res.Quality, res.Padded, res.Note)
	}
	if res.Style != StyleOriginal || res.Transformed {
		t.Fatalf("%s 必须走原始路径：style=%q transformed=%v", c.name, res.Style, res.Transformed)
	}
}

// 显式 original 与非法值回退，产物都必须与默认（零值）完全一致。
func TestOriginalGoldenExplicitStyleAndFallback(t *testing.T) {
	c := originalGolden[0]
	msgs := captureStyleWarnings(t)

	base, err := Build(c.spec)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, c, base)

	explicit := c.spec
	explicit.Style = StyleOriginal
	resExplicit, err := Build(explicit)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, c, resExplicit)

	invalid := c.spec
	invalid.Style = Style("vangogh")
	resInvalid, err := Build(invalid)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, c, resInvalid)
	if len(*msgs) != 1 || !strings.Contains((*msgs)[0], "vangogh") {
		t.Fatalf("非法风格应只告警一次并回退，实际 %v", *msgs)
	}
}

// 原始噪声渲染函数本身也未被改动。
func TestRenderRGBAPixelsUnchanged(t *testing.T) {
	img := renderRGBA(320, 240, 0.7, 12345)
	sum := sha256.Sum256(img.Pix)
	const want = "d4fabfd61d911f287aee359d33aedf8512783f836a119f01fb3dc2b85cf85880"
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("renderRGBA 像素已变化：%s，期望 %s", got, want)
	}
	if len(img.Pix) != 307200 {
		t.Fatalf("像素缓冲长度变化：%d", len(img.Pix))
	}
}
