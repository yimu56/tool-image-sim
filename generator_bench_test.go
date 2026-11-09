package main

import "testing"

// benchBuild 用递增种子模拟连续产线，避免任何跨帧缓存影响测量。
func benchBuild(b *testing.B, spec Spec) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		spec.Seed = int64(i)*7919 + 1
		if _, err := Build(spec); err != nil {
			b.Fatal(err)
		}
	}
}

// 默认配置：1280x960、目标 800KB、JPG、精确填充。
func BenchmarkBuildDefault(b *testing.B) {
	benchBuild(b, Spec{
		Width: 1280, Height: 960, TargetBytes: 800 * 1024,
		Format: FmtJPG, Pad: true, Quality: 88, AutoQuality: true,
	})
}

// 目标远大于可编码体积：主要靠元数据填充，二分最容易白跑的场景。
func BenchmarkBuildFillHeavy(b *testing.B) {
	benchBuild(b, Spec{
		Width: 320, Height: 240, TargetBytes: 512 * 1024,
		Format: FmtJPG, Pad: true, Quality: 88,
	})
}

// 目标小于高细节档位体积：必须靠二分收敛。
func BenchmarkBuildBinarySearch(b *testing.B) {
	benchBuild(b, Spec{
		Width: 320, Height: 240, TargetBytes: 60 * 1024,
		Format: FmtJPG, Pad: true, Quality: 88,
	})
}

func BenchmarkRenderRGBA(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = renderRGBA(1280, 960, 0.8, int64(i)+1)
	}
}

func BenchmarkEncodeJPEG(b *testing.B) {
	img := renderRGBA(1280, 960, 0.8, 42)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := encodeIMG(img, FmtJPG, 88); err != nil {
			b.Fatal(err)
		}
	}
}
