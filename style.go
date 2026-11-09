package main

import (
	"fmt"
	"image"
	"log"
	"strings"
)

// Style 是图片风格枚举。零值 "" 等价于 StyleOriginal：
// 旧配置、旧调用不传新参数时，行为与改造前完全一致。
type Style string

const (
	StyleOriginal  Style = "original"
	StyleMinecraft Style = "minecraft"
)

// transformFunc 是「原始噪声图生成之后」的本地后处理签名。
type transformFunc func(src *image.RGBA) *image.RGBA

// styles 同时决定配置合法值与界面下拉顺序，第一项必须是默认风格。
// 后续扩展新风格：加枚举常量 + 在 styleTransforms 注册转换函数 + 追加到这里。
var styles = []Style{StyleOriginal, StyleMinecraft}

// styleTransforms 是风格注册表。original 刻意不注册：它不做任何后处理，
// 因此也不存在「调用转换函数」的可能。
var styleTransforms = map[Style]transformFunc{
	StyleMinecraft: convertMinecraft,
}

// styleWarnf 是非法风格告警的统一出口，默认写标准日志；
// 程序启动阶段会替换成写运行日志的回调（GUI / -once 都能看到）。
// 只在启动时设置一次，之后只读。
var styleWarnf = func(msg string) {
	log.Printf("[style] %s", msg)
}

// StyleNames 返回界面下拉框使用的风格名列表，顺序与 styles 一致。
func StyleNames() []string {
	out := make([]string, len(styles))
	for i, s := range styles {
		out[i] = string(s)
	}
	return out
}

// parseStyle 把任意输入收敛到已知风格。ok=false 表示输入非法、已回退 original。
// 空字符串视为「未设置」，按默认 original 处理且不告警。
func parseStyle(raw string) (Style, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || s == string(StyleOriginal) {
		return StyleOriginal, true
	}
	for _, known := range styles {
		if s == string(known) {
			return known, true
		}
	}
	return StyleOriginal, false
}

// resolveStyle 解析风格，并对非法值输出明确的回退告警。
func resolveStyle(raw string) Style {
	s, ok := parseStyle(raw)
	if !ok {
		styleWarnf(fmt.Sprintf("未知图片风格 %q，已回退到默认风格 %q（可选值：%s）",
			raw, StyleOriginal, strings.Join(StyleNames(), "|")))
	}
	return s
}

// transformFor 返回该风格的后处理函数；original 返回 nil，表示原图直通。
func transformFor(s Style) transformFunc {
	if s == StyleOriginal {
		return nil
	}
	return styleTransforms[s]
}

// indexOfStyle 给界面下拉框用：未知值一律落到第一项 original。
func indexOfStyle(s Style) int {
	for i, known := range styles {
		if known == s {
			return i
		}
	}
	return 0
}
