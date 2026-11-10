package main

import (
	"fmt"
	"math"
)

const (
	// 估算时把大图等比缩小到这个像素数以内，再跑同一套 Build 体积探测，
	// 最后按面积比还原。合成噪声图的编码体积近似与像素数成正比，
	// 因此结果既精确又有界（约 10~30ms），可以直接在 UI 线程同步计算。
	estimateSamplePixels = 480 * 360
	// 估算专用固定种子，与产线 frameSeed 完全隔离，保证估算可复现，
	// 且不会消耗或改变任何真实生成的随机状态。
	estimateSeed = int64(0x5EED1234ABCD)
)

// EstimateSpec 是估算所需的完整参数快照。
type EstimateSpec struct {
	Width       int
	Height      int
	Format      string
	Quality     int
	Style       Style
	TargetKB    int
	Pad         bool
	AutoQuality bool
}

// SizeEstimate 是一个确定的单值预测（不再是区间）：
// ImageBytes 是图像数据编码体积，FileBytes 是最终落盘文件字节数。
type SizeEstimate struct {
	ImageBytes   int64 // 编码后、未填充的图像数据体积（受目标大小钳制）
	FileBytes    int64 // 预计最终文件字节数
	PaddedBytes  int64 // 元数据填充量
	NaturalBytes int64 // 满细节自然体积：不受目标大小钳制，只随分辨率/格式/质量/风格变化
	BelowMinimum bool  // 目标低于该尺寸可编码下限，无法精确命中
	BMPFixed     bool  // BMP 无压缩，体积由尺寸锁定
	Extrapolated bool  // 由缩小采样按面积还原
}

// EstimateSize 用「等比小采样 + 同一套 Build 探测 + 面积还原」预测单值：
// 采样图跑的是与真实生成完全相同的档位探测、质量二分和编码，所以预测口径
// 与真实产物一致；它只读取长度，不写文件，也不影响 original 路径。
func EstimateSize(spec EstimateSpec) (SizeEstimate, error) {
	return estimateSize(spec, estimateSamplePixels)
}

// estimateSize 的采样上限可注入，便于测试覆盖不同采样规模。
func estimateSize(spec EstimateSpec, sampleLimit int64) (SizeEstimate, error) {
	if spec.Width < 8 || spec.Height < 8 || spec.Width > 16000 || spec.Height > 16000 {
		return SizeEstimate{}, fmt.Errorf("尺寸需在 8 ~ 16000 之间")
	}
	if spec.TargetKB < 0 || spec.TargetKB > 200*1024 {
		return SizeEstimate{}, fmt.Errorf("目标大小需在 0 ~ 204800 KB 之间")
	}
	format := normalizeFormat(spec.Format)
	target := int64(spec.TargetKB) * 1024

	if format == FmtBMP {
		rowSize := (spec.Width*3 + 3) &^ 3
		n := int64(54 + rowSize*spec.Height)
		est := SizeEstimate{ImageBytes: n, FileBytes: n, NaturalBytes: n, BMPFixed: true}
		// BMP 的填充只是在像素数据后补零，同样受目标大小约束。
		if spec.Pad && target > n {
			est.FileBytes = target
			est.PaddedBytes = target - n
		} else if target > 0 && target < n {
			est.BelowMinimum = true
		}
		return est, nil
	}

	sw, sh := estimateSampleSize(spec.Width, spec.Height, sampleLimit)
	ratio := float64(int64(spec.Width)*int64(spec.Height)) / float64(int64(sw)*int64(sh))
	scaledTarget := int64(0)
	if target > 0 {
		scaledTarget = int64(math.Round(float64(target) / ratio))
		if scaledTarget < 1 {
			scaledTarget = 1
		}
		// 采样图的编码体积不可能超过原始 RGBA 数据量；把过大的目标收敛到
		// 这个上界，避免「目标 200MB」时为了填充白白申请一大块内存。
		if maxSample := int64(sw) * int64(sh) * 4; scaledTarget > maxSample {
			scaledTarget = maxSample
		}
	}

	st, _ := parseStyle(string(spec.Style)) // 非法值按 original 处理，不产生日志
	res, err := Build(Spec{
		Width: sw, Height: sh,
		TargetBytes: scaledTarget,
		Format:      format,
		Pad:         true, // 只为了加快探测收敛；下面只读 Encoded，不关心填充
		Quality:     spec.Quality,
		AutoQuality: spec.AutoQuality,
		Style:       st,
		Seed:        estimateSeed,
	})
	if err != nil {
		return SizeEstimate{}, err
	}

	imageBytes := int64(math.Round(float64(res.Encoded) * ratio))
	if imageBytes < int64(res.Encoded) {
		imageBytes = int64(res.Encoded)
	}
	// 采样图的编码体积超过缩放后的目标，说明原配置的目标低于可编码下限。
	// 不能用 res.Note 判断：非 original 风格会写体积说明备注。
	belowMinimum := scaledTarget > 0 && int64(res.Encoded) > scaledTarget

	// 额外算一次「满细节自然体积」：固定最高纹理档位、不做任何目标钳制，
	// 只由分辨率/格式/质量/风格决定。Build 的档位探测为了命中目标会主动
	// 降细节，导致 ImageBytes 在大分辨率下也被压到目标附近；这一项用来
	// 直观呈现分辨率本身对图像数据量的影响。
	natural, err := naturalEncoded(format, st, spec.Quality, sw, sh)
	if err != nil {
		return SizeEstimate{}, err
	}
	naturalBytes := int64(math.Round(float64(natural) * ratio))
	if naturalBytes < int64(natural) {
		naturalBytes = int64(natural)
	}

	est := SizeEstimate{
		ImageBytes:   imageBytes,
		FileBytes:    imageBytes,
		NaturalBytes: naturalBytes,
		BelowMinimum: belowMinimum,
		Extrapolated: sw != spec.Width || sh != spec.Height,
	}
	if spec.Pad && !belowMinimum && target > imageBytes {
		est.FileBytes = target
		est.PaddedBytes = target - imageBytes
	}
	return est, nil
}

// naturalEncoded 在采样图上以满细节档位（amp=1.0）渲染并编码一次，返回编码
// 字节数。它只读取长度、不写文件、不参与档位探测，因此不会改变 Build 的
// 预测口径，也不会扰动任何真实生成的随机状态。
func naturalEncoded(format string, style Style, quality, w, h int) (int, error) {
	img := renderRGBA(w, h, 1.0, estimateSeed)
	if tf := transformFor(style); tf != nil {
		img = tf(img)
	}
	data, err := encodeIMG(img, normalizeFormat(format), effectiveQuality(quality))
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// estimateSampleSize 把大尺寸等比收敛到估算采样上限内，保留宽高比。
func estimateSampleSize(w, h int, limit int64) (int, int) {
	total := int64(w) * int64(h)
	if total <= limit {
		return w, h
	}
	scale := math.Sqrt(float64(limit) / float64(total))
	sw := int(math.Round(float64(w) * scale))
	sh := int(math.Round(float64(h) * scale))
	if sw < 8 {
		sw = 8
	}
	if sh < 8 {
		sh = 8
	}
	return sw, sh
}

func formatSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.0f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/1048576)
	}
}

// Display 返回界面上一行展示的单值文本。
func (e SizeEstimate) Display() string {
	if e.BMPFixed {
		s := fmt.Sprintf("预计文件 %s（BMP 无压缩，由尺寸锁定）", formatSize(e.FileBytes))
		if e.PaddedBytes > 0 {
			s += fmt.Sprintf("，填充 %s", formatSize(e.PaddedBytes))
		}
		return s
	}
	s := fmt.Sprintf("预计文件 %s（图像数据 ≈ %s", formatSize(e.FileBytes), formatSize(e.ImageBytes))
	if e.PaddedBytes > 0 {
		s += fmt.Sprintf(" + 填充 %s", formatSize(e.PaddedBytes))
	}
	s += "）"
	if e.BelowMinimum {
		s += "；目标低于可编码下限，无法精确命中"
	}
	return s
}

// NaturalDisplay 返回「满细节自然体积」这一行的文案。该值不受目标大小
// 钳制，因此能直观看出分辨率变化带来的差异。
func (e SizeEstimate) NaturalDisplay() string {
	if e.NaturalBytes <= 0 {
		return "—"
	}
	if e.BMPFixed {
		return fmt.Sprintf("%s（BMP 无压缩，由尺寸锁定）", formatSize(e.NaturalBytes))
	}
	if e.NaturalBytes > e.ImageBytes {
		return fmt.Sprintf("%s（满细节；图像数据已自动降档至 %s 以命中目标）",
			formatSize(e.NaturalBytes), formatSize(e.ImageBytes))
	}
	return fmt.Sprintf("%s（满细节，未触发自动降档）", formatSize(e.NaturalBytes))
}
