package main

import (
	"image"
	"sync/atomic"
)

// minecraftTransformCalls 记录转换函数被调用的次数，供测试/自检确认
// original 路径从未进入任何后处理（生产逻辑不依赖它）。
var minecraftTransformCalls atomic.Int64

const (
	// minecraftBlockSize 是「一个方块单元」的边长（像素）。
	minecraftBlockSize = 8
	mcLevels           = 6 // 亮度档位数
	mcFamilies         = 3 // 每档的色相族：水/岩、土/草、沙/雪
	mcShades           = 5 // 每档的明暗变化数
	mcLumScale         = 210
)

// mcRGB 是程序化色板里的一个颜色。
type mcRGB struct{ r, g, b uint8 }

// minecraftPalette 是纯代码定义的近似色板，按亮度从暗到亮分档，
// 每档三个色相族亮度接近，保证量化后仍保留原图的主要明暗结构。
// 未使用任何 Minecraft 官方纹理素材，也不依赖网络或 AI 服务。
var minecraftPalette = [mcLevels][mcFamilies]mcRGB{
	{{28, 34, 56}, {46, 40, 36}, {38, 44, 50}},          // 深水/暗土/深板岩
	{{44, 78, 132}, {88, 74, 58}, {70, 74, 78}},         // 水/泥土/石
	{{58, 108, 170}, {112, 100, 70}, {96, 100, 94}},     // 浅水/棕土/灰岩
	{{86, 142, 196}, {110, 145, 76}, {130, 130, 124}},   // 亮水/草/浅石
	{{176, 196, 210}, {176, 196, 120}, {206, 190, 142}}, // 冰/浅草/沙
	{{226, 238, 244}, {222, 238, 196}, {238, 228, 196}}, // 雪/草甸/亮沙
}

var minecraftShadeDelta = [mcShades]int{-8, -4, 0, 4, 8}

// minecraftShades 预生成全部有限颜色（6 × 3 × 5 = 90 色），
// 保证转换结果始终是「有限调色板」，PNG/压缩体积也更可控。
var minecraftShades = buildMinecraftShades()

func buildMinecraftShades() [mcLevels][mcFamilies][mcShades]mcRGB {
	var out [mcLevels][mcFamilies][mcShades]mcRGB
	for l := 0; l < mcLevels; l++ {
		for f := 0; f < mcFamilies; f++ {
			c := minecraftPalette[l][f]
			for s, d := range minecraftShadeDelta {
				out[l][f][s] = mcRGB{
					r: clamp8(int32(c.r) + int32(d)),
					g: clamp8(int32(c.g) + int32(d)),
					b: clamp8(int32(c.b) + int32(d)),
				}
			}
		}
	}
	return out
}

// mixMinecraft 把块坐标与原图统计值混成确定性的 64 位哈希。
// 不引入任何全局随机源：同一张原始噪声图必然得到同一张风格图。
func mixMinecraft(bx, by, lum, warm uint64) uint64 {
	v := uint64(0x9E3779B97F4A7C15)
	for _, p := range [...]uint64{bx, by, lum, warm} {
		v ^= p + 0x9E3779B97F4A7C15 + (v << 6) + (v >> 2)
		v ^= v << 13
		v ^= v >> 7
		v ^= v << 17
	}
	return v
}

// convertMinecraft 把 renderRGBA 产出的原始噪声图做本地方块化转换：
//  1. 每 8×8 块取 RGB 平均值与亮度，决定色板亮度档；
//  2. 由块哈希 + 原图色偏决定色相族（水/岩、土/草、沙/雪）；
//  3. 块内用确定性 5 级明暗纹理 + 上沿提亮/下沿压暗做体素感；
//  4. 输出与输入同尺寸，整图颜色数不超过 90。
//
// 全程只依赖标准库，不改动原始随机源、不读取外部素材。
func convertMinecraft(src *image.RGBA) *image.RGBA {
	minecraftTransformCalls.Add(1)

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewRGBA(b)
	base := src.PixOffset(b.Min.X, b.Min.Y)
	ss, ds := src.Stride, out.Stride
	sp, dp := src.Pix, out.Pix

	const bs = minecraftBlockSize
	for by := 0; by < h; by += bs {
		bh := min(bs, h-by)
		for bx := 0; bx < w; bx += bs {
			bw := min(bs, w-bx)

			var sumR, sumG, sumB int
			for ly := 0; ly < bh; ly++ {
				i := base + (by+ly)*ss + bx*4
				for lx := 0; lx < bw; lx++ {
					sumR += int(sp[i])
					sumG += int(sp[i+1])
					sumB += int(sp[i+2])
					i += 4
				}
			}
			n := bw * bh
			avgR, avgG, avgB := sumR/n, sumG/n, sumB/n
			lum := (avgR*77 + avgG*150 + avgB*29) >> 8 // 整数 Rec.601 近似
			level := lum * mcLevels / mcLumScale
			if level >= mcLevels {
				level = mcLevels - 1
			}

			warm := uint64(avgR-avgB+256) / 8
			h64 := mixMinecraft(uint64(bx/bs), uint64(by/bs), uint64(lum), warm)
			family := int((h64 ^ (h64 >> 19) ^ warm) % mcFamilies)

			// 每块按自身哈希生成固定的块内纹理，无额外随机数消耗。
			var pattern [bs * bs]uint8
			v := h64
			for i := range pattern {
				v ^= v << 13
				v ^= v >> 7
				v ^= v << 17
				pattern[i] = uint8(v % mcShades)
			}

			for ly := 0; ly < bh; ly++ {
				di := (by+ly)*ds + bx*4
				for lx := 0; lx < bw; lx++ {
					shade := int(pattern[ly*bs+lx])
					if ly == 0 && shade < mcShades-1 {
						shade++ // 上沿高光
					}
					if ly == bh-1 && shade > 0 {
						shade-- // 下沿阴影
					}
					c := minecraftShades[level][family][shade]
					dp[di], dp[di+1], dp[di+2], dp[di+3] = c.r, c.g, c.b, 255
					di += 4
				}
			}
		}
	}
	return out
}
