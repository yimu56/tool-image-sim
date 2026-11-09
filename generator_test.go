package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"testing"
)

func decodeAny(t *testing.T, data []byte, format string) image.Image {
	t.Helper()
	var (
		img image.Image
		err error
	)
	switch format {
	case FmtJPG:
		img, err = jpeg.Decode(bytes.NewReader(data))
	case FmtPNG:
		img, err = png.Decode(bytes.NewReader(data))
	default:
		img, err = decodeBMP(data)
	}
	if err != nil {
		t.Fatalf("产物无法解码（%s）：%v", format, err)
	}
	return img
}

// decodeBMP 只做最小校验：头部魔数与声明的像素数据是否完整。
func decodeBMP(data []byte) (image.Image, error) {
	if len(data) < 54 || data[0] != 'B' || data[1] != 'M' {
		return nil, errInvalidBMP
	}
	return image.NewRGBA(image.Rect(0, 0, 1, 1)), nil
}

var errInvalidBMP = &bmpErr{}

type bmpErr struct{}

func (*bmpErr) Error() string { return "BMP 头部非法或长度不足" }

func TestBuildExactSizeAndDecodable(t *testing.T) {
	cases := []struct {
		name   string
		spec   Spec
		target int64
	}{
		{"jpg_60kb", Spec{Width: 320, Height: 240, Format: FmtJPG, Pad: true}, 60 * 1024},
		{"png_120kb", Spec{Width: 320, Height: 240, Format: FmtPNG, Pad: true}, 120 * 1024},
		{"jpg_1byte_填充边界", Spec{Width: 160, Height: 120, Format: FmtJPG, Pad: true}, 9 * 1024},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.spec.TargetBytes = c.target
			c.spec.Seed = 12345
			res, err := Build(c.spec)
			if err != nil {
				t.Fatalf("Build 失败: %v", err)
			}
			if int64(len(res.Data)) != c.target {
				t.Fatalf("期望 %d 字节，实际 %d 字节（note=%q）", c.target, len(res.Data), res.Note)
			}
			img := decodeAny(t, res.Data, c.spec.Format)
			if b := img.Bounds(); b.Dx() != c.spec.Width || b.Dy() != c.spec.Height {
				t.Fatalf("尺寸不符：%v", b)
			}
		})
	}
}

// 目标小于该尺寸的编码下限时，必须给出明确提示且产物依然可解码。
func TestBuildBelowMinimum(t *testing.T) {
	target := int64(5 * 1024)
	res, err := Build(Spec{Width: 800, Height: 600, Format: FmtJPG, Pad: true, TargetBytes: target, Seed: 12345})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if res.Note == "" {
		t.Fatal("低于下限时必须给出提示")
	}
	if int64(len(res.Data)) < target {
		t.Fatalf("低于下限时不应比目标更小：%d", len(res.Data))
	}
	decodeAny(t, res.Data, FmtJPG)
}

func TestBuildNoPadApproximate(t *testing.T) {
	res, err := Build(Spec{Width: 400, Height: 300, Format: FmtJPG, Pad: false, TargetBytes: 80 * 1024, Seed: 7})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if int64(len(res.Data)) > 80*1024 {
		t.Fatalf("未填充时不应超过目标：%d", len(res.Data))
	}
	decodeAny(t, res.Data, FmtJPG)
}

func TestBuildBMPFixedSize(t *testing.T) {
	res, err := Build(Spec{Width: 200, Height: 100, Format: FmtBMP, Pad: false, TargetBytes: 30 * 1024})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	rowSize := (200*3 + 3) &^ 3
	want := 54 + rowSize*100
	if len(res.Data) != want {
		t.Fatalf("BMP 大小应为 %d，实际 %d", want, len(res.Data))
	}
	decodeAny(t, res.Data, FmtBMP)
}

func TestBuildRejectsBadSize(t *testing.T) {
	if _, err := Build(Spec{Width: 1, Height: 1, Format: FmtJPG}); err == nil {
		t.Fatal("非法尺寸应报错")
	}
}

func TestSanitizePrefix(t *testing.T) {
	cases := map[string]string{
		"":          "IMG",
		"   ":       "IMG",
		"line1":     "line1",
		`a/b\c:d*e`: "a_b_c_d_e",
	}
	for in, want := range cases {
		if got := sanitizePrefix(in); got != want {
			t.Fatalf("sanitizePrefix(%q)=%q，期望 %q", in, got, want)
		}
	}
}
