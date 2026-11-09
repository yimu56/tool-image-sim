package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

// 质量必须由用户决定，且未开启自动降质量时不得被内部逻辑偷偷改掉。
func TestQualityIsRespected(t *testing.T) {
	encoded := map[int]int{}
	for _, q := range []int{20, 50, 95} {
		res, err := Build(Spec{
			Width: 480, Height: 360, TargetBytes: 200 * 1024,
			Format: FmtJPG, Pad: true, Quality: q, AutoQuality: false, Seed: 777,
		})
		if err != nil {
			t.Fatalf("quality=%d Build 失败: %v", q, err)
		}
		if res.Quality != q {
			t.Fatalf("质量应为用户设定的 %d，实际 %d", q, res.Quality)
		}
		decodeAny(t, res.Data, FmtJPG)
		encoded[q] = res.Encoded
	}
	if encoded[20] == encoded[95] {
		t.Fatal("不同质量应产生不同的编码体积")
	}
}

// 自动降质量开关：开启时能命中很小目标，关闭时尊重质量并给出明确提示。
func TestAutoQualitySwitch(t *testing.T) {
	base := Spec{
		Width: 800, Height: 600, TargetBytes: 30 * 1024,
		Format: FmtJPG, Pad: true, Quality: 100, Seed: 4242,
	}

	auto := base
	auto.AutoQuality = true
	resAuto, err := Build(auto)
	if err != nil {
		t.Fatal(err)
	}
	if resAuto.Quality >= 100 || strings.Contains(resAuto.Note, "请调低质量") {
		t.Fatalf("开启自动降质量后应能压到目标：quality=%d note=%q", resAuto.Quality, resAuto.Note)
	}
	decodeAny(t, resAuto.Data, FmtJPG)

	fixed := base
	fixed.AutoQuality = false
	resFixed, err := Build(fixed)
	if err != nil {
		t.Fatal(err)
	}
	if resFixed.Quality != 100 {
		t.Fatalf("关闭自动降质量后应保持质量 100，实际 %d", resFixed.Quality)
	}
	if !strings.Contains(resFixed.Note, "请调低质量") {
		t.Fatalf("命中不了目标时应给出明确提示，实际 note=%q", resFixed.Note)
	}
}

func TestEffectiveQuality(t *testing.T) {
	cases := map[int]int{0: defaultJPEGQuality, -1: defaultJPEGQuality, 1: 1, 100: 100, 150: 100}
	for in, want := range cases {
		if got := effectiveQuality(in); got != want {
			t.Fatalf("effectiveQuality(%d)=%d，期望 %d", in, got, want)
		}
	}
}

// 同一纳秒、不同序号也必须拿到不同种子。
func TestFrameSeedUniquePerSequence(t *testing.T) {
	const nano = int64(1700000000000000000)
	seen := map[int64]bool{}
	for seq := int64(1); seq <= 64; seq++ {
		s := frameSeed(nano, seq)
		if seen[s] {
			t.Fatalf("序号 %d 出现种子碰撞", seq)
		}
		seen[s] = true
	}
}

// 相同种子应可复现（用于定位问题），不同种子必须画面不同。
func TestSeedDeterminism(t *testing.T) {
	a := renderRGBA(160, 120, 0.7, 12345)
	b := renderRGBA(160, 120, 0.7, 12345)
	c := renderRGBA(160, 120, 0.7, 12346)
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Fatal("相同种子应产生完全相同画面")
	}
	if bytes.Equal(a.Pix, c.Pix) {
		t.Fatal("不同种子不应产生相同画面")
	}
}

// 审计：模拟真实产线连续产 12 帧（细节档位打满 + 精确填充），
// 原始像素与最终文件字节都必须两两不同。
func TestFramesAreNeverIdentical(t *testing.T) {
	const n = 12
	pixelHashes := map[string]int{}
	fileHashes := map[string]int{}
	baseNano := int64(1700000000000000000)

	for i := 1; i <= n; i++ {
		seed := frameSeed(baseNano+int64(i)*1000, int64(i))

		img := renderRGBA(320, 240, 1.0, seed)
		ph := fmt.Sprintf("%x", sha256.Sum256(img.Pix))
		if prev, ok := pixelHashes[ph]; ok {
			t.Fatalf("第 %d 帧与第 %d 帧画面完全一致", i, prev)
		}
		pixelHashes[ph] = i

		res, err := Build(Spec{
			Width: 320, Height: 240, TargetBytes: 512 * 1024,
			Format: FmtJPG, Pad: true, Quality: 88, AutoQuality: true, Seed: seed,
		})
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(res.Data)) != 512*1024 {
			t.Fatalf("第 %d 帧大小应为 524288，实际 %d", i, len(res.Data))
		}
		fh := fmt.Sprintf("%x", sha256.Sum256(res.Data))
		if prev, ok := fileHashes[fh]; ok {
			t.Fatalf("第 %d 帧与第 %d 帧文件字节完全一致", i, prev)
		}
		fileHashes[fh] = i
		decodeAny(t, res.Data, FmtJPG)
	}
}
