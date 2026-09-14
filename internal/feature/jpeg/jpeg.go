// Package jpeg 判断一块簇数据像不像 JPEG。
// 第一块看 magic（FF D8 FF，精确）；后续碎片只看熵编码流的统计特征（宽松，只说明「像」）。
package jpeg

import "math"

// IsJPEGHeader 判断 data 是否以 JPEG 文件头开头。
func IsJPEGHeader(data []byte) bool {
	return len(data) >= 3 &&
		data[0] == 0xFF &&
		data[1] == 0xD8 &&
		data[2] == 0xFF
}

// Feature 是一个簇里 JPEG 熵编码流的统计特征。
// BlockSize：原始输入数据字节长度，由Scan填充，避免外部传入total传错。
// 注意：若0xFF落在本块最后一字节，紧随的0x00在下一簇，则StuffedCount会被低估（磁盘雕刻固有边界问题）。
type Feature struct {
	BlockSize    int     // 输入块字节大小，Scan自动填充
	FFCount      int     // 0xFF 总个数
	StuffedCount int     // FF 00：熵编码里 0xFF 后必须补 0x00
	RestartCount int     // FF D0~D7：重启标记，只出现在熵编码流里
	Entropy      float64 // 字节熵（bit/byte），调参用，当前不参与判决
}

// Scan 统计 data 的 JPEG 特征，一遍扫完。
func Scan(data []byte) Feature {
	var f Feature
	f.BlockSize = len(data)
	if len(data) == 0 {
		return f
	}

	var freq [256]int

	for i := 0; i < len(data); i++ {
		freq[data[i]]++
		if data[i] != 0xFF {
			continue
		}
		f.FFCount++
		if i+1 >= len(data) {
			continue // FF 在簇末尾，下一个字节在下一簇，无法判定是否 FF‑00 stuffing
		}
		switch data[i+1] {
		case 0x00:
			f.StuffedCount++
		case 0xD0, 0xD1, 0xD2, 0xD3, 0xD4, 0xD5, 0xD6, 0xD7:
			f.RestartCount++
		}
	}

	f.Entropy = shannonEntropy(freq[:], len(data))
	return f
}

func shannonEntropy(freq []int, total int) float64 {
	if total == 0 {
		return 0
	}
	entropy := 0.0
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / float64(total)
		entropy -= p * math.Log2(p)
	}
	return entropy
}

// IsLikelyJPEG 判断后续碎片像不像 JPEG。
//
// 核心证据只有一个：FF 00 字节填充的密度。真实 JPEG 熵流里每个 0xFF 后面必须
// 补一个 0x00，0xFF 频率约 1/256，256KB 簇里期望 ~1000 个 FF 00；随机数据
// （视频、HEIC、加密、任何压缩流）只有 total/65536，256KB 里 ~4 个。
// 阈值 1/2048 取在两者之间，离两边都有 8~32 倍余量；视频流靠 00 00 03 防竞争、
// 不做 FF 00 填充，正好被这条排除。
//
// 重启标记（FF D0~D7）不能当证据：8 种标记 × 每字节对 1/65536，随机数据 256KB
// 里期望 ~32 个，比真 JPEG（多数编码器不开重启间隔）还多，判了就是误报。
//
// 这里只做候选筛选；「不像」读作「目前不像」，不是证明不是。
//
// 调用方式：feat := Scan(block); ok := IsLikelyJPEG(feat)
func IsLikelyJPEG(f Feature) bool {
	total := f.BlockSize
	if total == 0 {
		return false
	}
	return f.StuffedCount >= 16 &&
		float64(f.StuffedCount)/float64(total) >= 1.0/2048.0
}
