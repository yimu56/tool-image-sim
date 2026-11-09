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

	// defaultJPEGQuality 是 Spec.Quality 未指定时的内部兜底，不是界面默认值
	// （界面默认值见 config.go 的 defaultConfigQuality）。
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
	Style       Style // 图片风格；零值或 original 表示原始风格，不做任何后处理
}

// renderFunc 是「按细节档位渲染原始噪声图」的函数签名。
// 原始风格下它直接指向 renderRGBA 本身，不经过任何包装。
type renderFunc func(w, h int, amp float64, seed int64) *image.RGBA

// Result 是生成结果。
type Result struct {
	Data    []byte
	Encoded int     // 编码后、未填充时的真实图像数据大小
	Detail  float64 // 实际使用的纹理细节强度 0~1
	Quality int     // JPEG 质量（PNG/BMP 无意义）
	Padded  bool    // 是否做过填充
	Note    string
	Style   Style // 本次实际采用的风格
	// Transformed 表示是否执行过风格后处理；原始风格恒为 false。
	Transformed bool
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

// Build 按 spec 生成图片字节。默认风格（Style 零值或 "original"）走完全
// 未改动的原始链路：渲染函数直接是 renderRGBA 本身，不注册任何后处理，
// 因此不会调用 Minecraft 转换函数，产物与改造前逐字节一致。
// 其他风格只在「原始噪声图渲染之后」插入本地转换，档位探测、质量二分、
// 元数据填充全部基于转换后的编码结果，尺寸与体积语义保持不变。
func Build(spec Spec) (*Result, error) {
	style := resolveStyle(string(spec.Style))
	if style == StyleOriginal {
		res, err := build(spec, renderRGBA)
		if err != nil {
			return nil, err
		}
		res.Style = StyleOriginal
		return res, nil
	}
	tf := transformFor(style)
	if tf == nil {
		// 预留扩展：枚举存在但没有转换实现时明确回退，不静默产出。
		styleWarnf(fmt.Sprintf("风格 %q 暂无转换实现，本次回退到 %q", style, StyleOriginal))
		res, err := build(spec, renderRGBA)
		if err != nil {
			return nil, err
		}
		res.Style = StyleOriginal
		return res, nil
	}
	render := func(w, h int, amp float64, seed int64) *image.RGBA {
		return tf(renderRGBA(w, h, amp, seed))
	}
	res, err := build(spec, render)
	if err != nil {
		return nil, err
	}
	res.Style = style
	res.Transformed = true
	explainStyleSize(res, style, spec)
	return res, nil
}

// explainStyleSize 把风格转换对文件体积的影响写进日志备注：方块化 / 有限调色板
// 会显著提高压缩率，缺口沿用原有元数据填充补齐，不静默改变「目标大小」语义。
func explainStyleSize(res *Result, style Style, spec Spec) {
	if spec.TargetBytes <= 0 {
		return // 未限制大小，没有体积语义需要说明
	}
	padded := len(res.Data) - res.Encoded
	gap := spec.TargetBytes - int64(len(res.Data))
	if padded <= 0 && gap <= 0 {
		return
	}
	msg := fmt.Sprintf("风格 %s：方块化后图像数据编码为 %d 字节", style, res.Encoded)
	switch {
	case padded > 0 && spec.Pad && res.Padded:
		msg += fmt.Sprintf("，已用原有元数据填充补齐 %d 字节到目标（尺寸不变）", padded)
	case gap > 0:
		msg += fmt.Sprintf("，比目标小 %d 字节，未做填充", gap)
	default:
		return
	}
	if res.Note == "" {
		res.Note = msg
	} else {
		res.Note = msg + "；" + res.Note
	}
}

// build 是原有生成链路，核心策略未改动：
//  1. 以中点档位为锚做自适应探测，只在必要时在对应区间二分，
//     用最少的「渲染 + 编码」次数找到体积不超过目标、且尽量接近目标的纹理档位；
//  2. 若最平滑仍超目标且 AutoQuality=true（仅 JPEG），再对质量二分自动下调；
//  3. 仍小于目标时，用合法的元数据块（PNG tEXt / JPEG COM）填充到精确字节数，
//     保证产出的文件既符合图片格式，字节数又严格等于用户设定值。
//
// render 参数只在非原始风格时包裹一层本地转换；原始风格传入的就是 renderRGBA。
func build(spec Spec, render renderFunc) (*Result, error) {
	if spec.Width < 8 || spec.Height < 8 {
		return nil, fmt.Errorf("尺寸太小：%dx%d（最小 8x8）", spec.Width, spec.Height)
	}
	if spec.Width > 16000 || spec.Height > 16000 {
		return nil, fmt.Errorf("尺寸过大：%dx%d（最大 16000x16000）", spec.Width, spec.Height)
	}
	format := normalizeFormat(spec.Format)
	quality := effectiveQuality(spec.Quality)

	if format == FmtBMP {
		return buildBMP(spec, render)
	}

	target := spec.TargetBytes
	if target <= 0 {
		img := render(spec.Width, spec.Height, 0.50, spec.Seed)
		data, err := encodeIMG(img, format, quality)
		if err != nil {
			return nil, err
		}
		return &Result{Data: data, Encoded: len(data), Detail: 0.50, Quality: quality}, nil
	}

	// 每次档位探测都要全量渲染 + 编码，是整条链路最贵的操作。
	// 开启精确填充时容差放宽到 10%：缺的字节本来就要靠元数据补齐，
	// 为最后几个百分点再跑几次全量编码并不划算；不填充时保持 2.5%，
	// 让实际文件体积尽量贴近设定值。
	tolerance := target / 40
	if spec.Pad {
		tolerance = target / 10
	}
	probe := func(level int) ([]byte, error) {
		img := render(spec.Width, spec.Height, float64(level)/110.0, spec.Seed)
		return encodeIMG(img, format, quality)
	}

	var best []byte
	bestDetail, bestQuality := 0.0, quality
	// accept 记录一次命中，并判断剩余缺口是否已小到可以交给填充
	accept := func(data []byte, level int) bool {
		if int64(len(data)) > target {
			return false
		}
		best, bestDetail, bestQuality = data, float64(level)/110.0, quality
		return target-int64(len(data)) < tolerance
	}

	// 先用中点 55 探测：一次测量就能区分"目标偏小"（往低档位找）、
	// "已经够接近"（直接收工）、"还有余量"（往高档位找）三种情况。
	const pivot = 55
	d, err := probe(pivot)
	if err != nil {
		return nil, err
	}
	lo, hi := 0, -1 // 默认无需继续搜索
	switch {
	case int64(len(d)) > target:
		lo, hi = 0, pivot-1
	case accept(d, pivot):
		// 一次命中即可，不进入下面的循环
	default:
		// 余量还很大，先试最高档；若它也不超目标，就是最优解（等价于二分跑满）
		top, err := probe(110)
		if err != nil {
			return nil, err
		}
		if int64(len(top)) <= target {
			best, bestDetail, bestQuality = top, 1, quality
		} else {
			best, bestDetail = d, float64(pivot)/110.0
			lo, hi = pivot+1, 109 // 目标落在中点与最高档之间
		}
	}

	for lo <= hi {
		mid := (lo + hi) / 2
		data, err := probe(mid)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) <= target {
			if accept(data, mid) {
				break
			}
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}

	if best == nil {
		// 最平滑画面与质量无关，只渲染一次即可供下面所有质量档位复用
		flat := render(spec.Width, spec.Height, 0, spec.Seed)
		if format == FmtJPG && spec.AutoQuality {
			qlo, qhi := 1, quality
			for i := 0; i < 7 && qlo <= qhi; i++ {
				mid := (qlo + qhi) / 2
				data, err := encodeIMG(flat, format, mid)
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
			data, err := encodeIMG(flat, format, q)
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

func buildBMP(spec Spec, render renderFunc) (*Result, error) {
	img := render(spec.Width, spec.Height, 0.55, spec.Seed)
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
// 每个轴的整数坐标与插值权重只跟坐标有关，预先算成表，
// 内层逐像素循环里就只剩数组索引和乘加，不再有除法和分支。
type noiseGrid struct {
	gw, gh int
	v      []float64
	x0, y0 []int
	sx, sy []float64
}

func newNoiseGrid(w, h, cell int, s *uint64) *noiseGrid {
	g := &noiseGrid{gw: w/cell + 2, gh: h/cell + 2}
	g.v = make([]float64, g.gw*g.gh)
	for i := range g.v {
		*s ^= *s << 13
		*s ^= *s >> 7
		*s ^= *s << 17
		g.v[i] = float64((*s>>40)&0xFFF) / 4095.0
	}
	g.x0, g.sx = axisTable(w, g.gw, float64(cell))
	g.y0, g.sy = axisTable(h, g.gh, float64(cell))
	return g
}

func axisTable(n, glen int, cell float64) ([]int, []float64) {
	i0 := make([]int, n)
	w := make([]float64, n)
	for i := 0; i < n; i++ {
		g := float64(i) / cell
		x0 := int(g)
		if x0 > glen-2 {
			x0 = glen - 2
		}
		f := g - float64(x0)
		if f > 1 {
			f = 1
		}
		i0[i] = x0
		w[i] = f * f * (3 - 2*f)
	}
	return i0, w
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

	coarse := newNoiseGrid(w, h, 48, &s) // 大块明暗
	fine := newNoiseGrid(w, h, 14, &s)   // 细节起伏
	cv, fv := coarse.v, fine.v
	cgw, fgw := coarse.gw, fine.gw
	na := int32(amp * 110)
	// 细节层权重随档位增长：低档位保持平滑（让"小文件"也能生成），高档位纹理丰富
	fw := 0.06 + 0.34*amp
	cw := 1 - fw

	for y := 0; y < h; y++ {
		row := y * stride
		cA, cB := coarse.y0[y]*cgw, (coarse.y0[y]+1)*cgw
		fA, fB := fine.y0[y]*fgw, (fine.y0[y]+1)*fgw
		csy, fsy := coarse.sy[y], fine.sy[y]
		for x := 0; x < w; x++ {
			cx := coarse.x0[x]
			csx := coarse.sx[x]
			top := cv[cA+cx] + (cv[cA+cx+1]-cv[cA+cx])*csx
			bot := cv[cB+cx] + (cv[cB+cx+1]-cv[cB+cx])*csx
			c := top + (bot-top)*csy

			fx := fine.x0[x]
			fsx := fine.sx[x]
			top = fv[fA+fx] + (fv[fA+fx+1]-fv[fA+fx])*fsx
			bot = fv[fB+fx] + (fv[fB+fx+1]-fv[fB+fx])*fsx
			f := c*cw + (top+(bot-top)*fsy)*fw

			base := int32(25) + int32(f*170)

			// 随机流就地展开，避免逐像素闭包调用；消耗次数与建网格时一致
			s ^= s << 13
			s ^= s >> 7
			s ^= s << 17
			r := s
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
		growEncodeBuffer(&buf, img)
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
		// 压缩后的体积通常不超过像素数，一次把缓冲开够，
		// 避免编码过程中反复扩容 + 整块拷贝。
		growEncodeBuffer(&buf, img)
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func growEncodeBuffer(buf *bytes.Buffer, img image.Image) {
	b := img.Bounds()
	n := b.Dx() * b.Dy()
	if n <= 0 {
		return
	}
	// 极限尺寸（最大 16000x16000）下一字节一像素会一次性申请 256MB，
	// 封顶 32MB，超大图交给 bytes.Buffer 自己按需扩容。
	const maxPrealloc = 32 << 20
	if n > maxPrealloc {
		n = maxPrealloc
	}
	buf.Grow(n)
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
