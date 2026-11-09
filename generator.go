package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
)

const (
	FmtJPG = "JPG"
	FmtPNG = "PNG"
	FmtBMP = "BMP"

	defaultJPEGQuality = 88
)

// Spec 描述一张待生成图片的参数。
type Spec struct {
	Width       int
	Height      int
	TargetBytes int64 // 0 表示不限制，按中等细节输出
	Format      string
	Pad         bool // 是否用元数据块把文件填充到精确的目标字节数
	Quality     int  // JPEG 质量 1~100；<=0 视为默认 88；PNG/BMP 忽略
	AutoQuality bool // 最平滑仍超目标时，是否允许自动下调质量
	Seed        int64
}

// Result 是生成结果。
type Result struct {
	Data    []byte
	Encoded int     // 编码后、未填充时的真实图像数据大小
	Detail  float64 // 实际使用的纹理细节强度 0~1
	Quality int     // JPEG 质量（PNG/BMP 无意义）
	Padded  bool    // 是否做过填充
	Note    string
}

// effectiveQuality 把用户输入收敛到合法 JPEG 质量；未填时用默认值。
func effectiveQuality(q int) int {
	if q <= 0 {
		return defaultJPEGQuality
	}
	if q > 100 {
		return 100
	}
	return q
}

func normalizeFormat(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "PNG":
		return FmtPNG
	case "BMP":
		return FmtBMP
	default:
		return FmtJPG
	}
}

func extOf(format string) string {
	return strings.ToLower(normalizeFormat(format))
}

// Build 按 spec 生成图片字节。核心策略：
//  1. 用 spec.Quality 指定的 JPEG 质量（PNG/BMP 忽略），对"纹理细节档位"做整数二分，
//     让编码后的体积逼近目标且不超过目标；
//  2. 若最平滑仍超目标且 AutoQuality=true（仅 JPEG），再对质量二分自动下调；
//  3. 仍小于目标时，用合法的元数据块（PNG tEXt / JPEG COM）填充到精确字节数，
//     保证产出的文件既符合图片格式，字节数又严格等于用户设定值。
func Build(spec Spec) (*Result, error) {
	if spec.Width < 8 || spec.Height < 8 {
		return nil, fmt.Errorf("尺寸太小：%dx%d（最小 8x8）", spec.Width, spec.Height)
	}
	if spec.Width > 16000 || spec.Height > 16000 {
		return nil, fmt.Errorf("尺寸过大：%dx%d（最大 16000x16000）", spec.Width, spec.Height)
	}
	format := normalizeFormat(spec.Format)
	quality := effectiveQuality(spec.Quality)

	if format == FmtBMP {
		return buildBMP(spec)
	}

	target := spec.TargetBytes
	if target <= 0 {
		img := renderRGBA(spec.Width, spec.Height, 0.50, spec.Seed)
		data, err := encodeIMG(img, format, quality)
		if err != nil {
			return nil, err
		}
		return &Result{Data: data, Encoded: len(data), Detail: 0.50, Quality: quality}, nil
	}

	// 对"纹理档位"整数二分（0~110 对应 amp 0~1），保证一定能降到 0，
	// 用浮点的话会因为量化精度停在小数上，误判"压不到目标"。
	lo, hi := 0, 110
	var best []byte
	bestDetail, bestQuality := 0.0, quality

	for lo <= hi {
		mid := (lo + hi) / 2
		amp := float64(mid) / 110.0
		img := renderRGBA(spec.Width, spec.Height, amp, spec.Seed)
		data, err := encodeIMG(img, format, quality)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) <= target {
			best, bestDetail, bestQuality = data, amp, quality
			lo = mid + 1
			// 已经足够接近目标（误差 < 2.5%），剩余交给填充
			if target-int64(len(data)) < target/40 {
				break
			}
		} else {
			hi = mid - 1
		}
	}

	if best == nil {
		// 最平滑仍超目标：仅当允许自动降质量时，JPEG 才有第二级手段
		if format == FmtJPG && spec.AutoQuality {
			qlo, qhi := 1, quality
			for i := 0; i < 7 && qlo <= qhi; i++ {
				mid := (qlo + qhi) / 2
				img := renderRGBA(spec.Width, spec.Height, 0, spec.Seed)
				data, err := encodeIMG(img, format, mid)
				if err != nil {
					return nil, err
				}
				if int64(len(data)) <= target {
					best, bestDetail, bestQuality = data, 0, mid
					qlo = mid + 1
				} else {
					qhi = mid - 1
				}
			}
		}
		if best == nil {
			// 自动降质量已开：用最低质量出最小文件；否则尊重用户设定，明确告知命中不了
			q := quality
			if spec.AutoQuality && format == FmtJPG {
				q = 1
			}
			img := renderRGBA(spec.Width, spec.Height, 0, spec.Seed)
			data, err := encodeIMG(img, format, q)
			if err != nil {
				return nil, err
			}
			note := fmt.Sprintf("目标低于该尺寸可编码下限（最小约 %d KB），已输出最小文件", (len(data)+1023)/1024)
			if format == FmtJPG && !spec.AutoQuality {
				note = fmt.Sprintf("质量 %d 下最小约 %d KB，超过目标；请调低质量、放大目标或勾选自动降质量",
					q, (len(data)+1023)/1024)
			}
			return &Result{
				Data: data, Encoded: len(data), Detail: 0, Quality: q, Note: note,
			}, nil
		}
	}

	res := &Result{Data: best, Encoded: len(best), Detail: bestDetail, Quality: bestQuality}
	if int64(len(best)) < target {
		short := target - int64(len(best))
		if spec.Pad {
			switch format {
			case FmtPNG:
				res.Data = padPNG(best, short)
			case FmtJPG:
				res.Data = padJPEG(best, short)
			}
			res.Padded = true
		} else {
			res.Note = fmt.Sprintf("未填充，比目标小 %d 字节", short)
		}
	}
	return res, nil
}

func buildBMP(spec Spec) (*Result, error) {
	img := renderRGBA(spec.Width, spec.Height, 0.55, spec.Seed)
	data, err := encodeBMP(img)
	if err != nil {
		return nil, err
	}
	res := &Result{Data: data, Encoded: len(data), Detail: 0.55}
	if spec.TargetBytes > 0 && int64(len(data)) != spec.TargetBytes {
		if spec.Pad && spec.TargetBytes > int64(len(data)) {
			data = append(data, make([]byte, spec.TargetBytes-int64(len(data)))...)
			res.Data, res.Padded = data, true
		} else {
			res.Note = fmt.Sprintf("BMP 未压缩，大小由尺寸决定（%d KB）", len(data)/1024)
		}
	}
	return res, nil
}

// noiseGrid 是一个随机值网格，按 smoothstep 双线性插值采样，
// 用来合成"低频斑块 + 高频起伏"的自然纹理。
type noiseGrid struct {
	gw, gh int
	cell   float64
	v      []float64
}

func newNoiseGrid(w, h, cell int, next func() uint64) *noiseGrid {
	g := &noiseGrid{
		gw:   w/cell + 2,
		gh:   h/cell + 2,
		cell: float64(cell),
	}
	g.v = make([]float64, g.gw*g.gh)
	for i := range g.v {
		g.v[i] = float64((next()>>40)&0xFFF) / 4095.0
	}
	return g
}

func (g *noiseGrid) sample(x, y int) float64 {
	gx := float64(x) / g.cell
	x0 := int(gx)
	if x0 > g.gw-2 {
		x0 = g.gw - 2
	}
	fx := gx - float64(x0)
	if fx > 1 {
		fx = 1
	}
	sx := fx * fx * (3 - 2*fx)

	gy := float64(y) / g.cell
	y0 := int(gy)
	if y0 > g.gh-2 {
		y0 = g.gh - 2
	}
	fy := gy - float64(y0)
	if fy > 1 {
		fy = 1
	}
	sy := fy * fy * (3 - 2*fy)

	rowA := y0 * g.gw
	rowB := (y0 + 1) * g.gw
	v00, v10 := g.v[rowA+x0], g.v[rowA+x0+1]
	v01, v11 := g.v[rowB+x0], g.v[rowB+x0+1]
	top := v00 + (v10-v00)*sx
	bot := v01 + (v11-v01)*sx
	return top + (bot-top)*sy
}

// renderRGBA 生成一张"类相机拍摄"的图：两层随机斑块（模拟光照不均与表面起伏）
// + 可控强度的高频噪声。amp 越大，压缩后体积越大；每帧斑块随机，画面各不相同。
func renderRGBA(w, h int, amp float64, seed int64) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	pix := img.Pix
	stride := img.Stride

	s := uint64(seed)*6364136223846793005 + 1442695040888963407
	if s == 0 {
		s = 0x9E3779B97F4A7C15
	}
	next := func() uint64 {
		s ^= s << 13
		s ^= s >> 7
		s ^= s << 17
		return s
	}

	coarse := newNoiseGrid(w, h, 48, next) // 大块明暗
	fine := newNoiseGrid(w, h, 14, next)   // 细节起伏
	na := int32(amp * 110)
	// 细节层权重随档位增长：低档位保持平滑（让"小文件"也能生成），高档位纹理丰富
	fw := 0.06 + 0.34*amp
	cw := 1 - fw

	for y := 0; y < h; y++ {
		row := y * stride
		for x := 0; x < w; x++ {
			f := coarse.sample(x, y)*cw + fine.sample(x, y)*fw
			base := int32(25) + int32(f*170)

			r := next()
			n := int32((r>>56)&0xFF) - 128 // -128..127
			j := n * na / 100              // 亮度噪声，amp=0 时为 0
			v := base + j
			if v < 0 {
				v = 0
			} else if v > 255 {
				v = 255
			}
			// 独立的弱色噪声：让彩色通道不相关，更像真实相机
			cr := int32(int8(byte(r>>48))) * na / 500
			cb := int32(int8(byte(r>>40))) * na / 500

			i := row + x*4
			pix[i] = clamp8(v + 12 + cr)  // R
			pix[i+1] = clamp8(v)          // G
			pix[i+2] = clamp8(v - 8 + cb) // B
			pix[i+3] = 255
		}
	}
	return img
}

func clamp8(v int32) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func encodeIMG(img image.Image, format string, quality int) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case FmtPNG:
		// DefaultCompression：体积明显小于 BestSpeed，编码耗时仍可接受
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		if err := enc.Encode(&buf, img); err != nil {
			return nil, err
		}
	case FmtBMP:
		rgba, ok := img.(*image.RGBA)
		if !ok {
			return nil, fmt.Errorf("BMP 编码需要 RGBA 图像")
		}
		data, err := encodeBMP(rgba)
		if err != nil {
			return nil, err
		}
		return data, nil
	default:
		if quality < 1 {
			quality = 1
		}
		if quality > 100 {
			quality = 100
		}
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// encodeBMP 手写 24 位 BMP（无压缩，体积固定）。
func encodeBMP(img *image.RGBA) ([]byte, error) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	rowSize := (w*3 + 3) &^ 3
	dataSize := rowSize * h
	buf := make([]byte, 54+dataSize)

	buf[0], buf[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(buf[2:], uint32(len(buf)))
	binary.LittleEndian.PutUint32(buf[10:], 54)
	binary.LittleEndian.PutUint32(buf[14:], 40)
	binary.LittleEndian.PutUint32(buf[18:], uint32(w))
	binary.LittleEndian.PutUint32(buf[22:], uint32(h))
	binary.LittleEndian.PutUint16(buf[26:], 1)
	binary.LittleEndian.PutUint16(buf[28:], 24)
	binary.LittleEndian.PutUint32(buf[34:], uint32(dataSize))
	binary.LittleEndian.PutUint32(buf[38:], 2835)
	binary.LittleEndian.PutUint32(buf[42:], 2835)

	for y := 0; y < h; y++ {
		src := (h - 1 - y) * img.Stride
		dst := 54 + y*rowSize
		for x := 0; x < w; x++ {
			buf[dst+x*3] = img.Pix[src+x*4+2]
			buf[dst+x*3+1] = img.Pix[src+x*4+1]
			buf[dst+x*3+2] = img.Pix[src+x*4]
		}
	}
	return buf, nil
}

// padPNG 在 IEND 之前插入一个 tEXt 块，把文件补到精确长度。
// tEXt 是标准块，解码器会正常跳过，图片依然可打开。
func padPNG(d []byte, need int64) []byte {
	const overhead = 12 // 长度4 + 类型4 + CRC4
	keyword := []byte("Comment\x00")

	if need < int64(overhead+len(keyword)) || len(d) < 12 {
		return append(append([]byte(nil), d...), make([]byte, need)...)
	}
	fill := need - int64(overhead+len(keyword))
	body := make([]byte, 0, int64(len(keyword))+fill)
	body = append(body, keyword...)
	body = append(body, make([]byte, fill)...)

	chunk := make([]byte, 0, int64(overhead)+int64(len(body)))
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(body)))
	chunk = append(chunk, l[:]...)
	chunk = append(chunk, 't', 'E', 'X', 't')
	chunk = append(chunk, body...)
	c := crc32.NewIEEE()
	_, _ = c.Write([]byte("tEXt"))
	_, _ = c.Write(body)
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], c.Sum32())
	chunk = append(chunk, sum[:]...)

	out := make([]byte, 0, len(d)+len(chunk))
	out = append(out, d[:len(d)-12]...)
	out = append(out, chunk...)
	out = append(out, d[len(d)-12:]...)
	return adjustExact(out, need+int64(len(d)))
}

// padJPEG 在 SOI 之后插入若干 COM（注释）段，把文件补到精确长度。
// 单段上限 65533 字节载荷，超出自动分段。
func padJPEG(d []byte, need int64) []byte {
	if len(d) < 4 {
		return append(append([]byte(nil), d...), make([]byte, need)...)
	}
	remaining := need
	var segs []byte
	for remaining >= 4 {
		payload := remaining - 4
		if payload > 65533 {
			payload = 65533
		}
		segs = append(segs, 0xFF, 0xFE)
		l := uint16(payload + 2)
		segs = append(segs, byte(l>>8), byte(l))
		segs = append(segs, make([]byte, payload)...)
		remaining -= 4 + payload
	}

	out := make([]byte, 0, len(d)+len(segs))
	out = append(out, d[:2]...) // SOI
	out = append(out, segs...)
	out = append(out, d[2:]...)
	for remaining > 0 { // 剩余 1~3 字节，直接追加到文件尾（解码器忽略）
		out = append(out, 0)
		remaining--
	}
	return out
}

// adjustExact 兜底：把切片长度校正到 want（只可能是补零）。
func adjustExact(d []byte, want int64) []byte {
	for int64(len(d)) < want {
		d = append(d, 0)
	}
	return d
}
