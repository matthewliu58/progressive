// Package jpeg 判断一簇数据像不像 JPEG，并给「两簇是否相邻」打分，供文件雕刻使用。
//
// 设计前提：
//   - 不保留原始簇数据。整卡空闲簇上百万，留在内存里就是几十 GB；
//   - Scan 从一簇里提取出紧凑的 Feature，随即可以丢掉原始数据；
//   - IsLikelyJPEG 只做宽松的候选筛选，不是 JPEG 校验器；
//   - ScoreNext(a, b) 估计「b 紧跟在 a 后面」的可能性有多大。
//
// 这个包不能证明两簇属于同一个 JPEG —— 熵编码流里任意字节都能接任意字节，
// 它只能给上层的雕刻算法提供证据（能用来否决，不能用来确认）。
package jpeg

import "math"

// IsJPEGHeader 判断 data 是不是以 JPEG 的起始标记开头，即：
//
//	FF D8 FF
//
// 只适用于文件的第一簇。后续碎片本来就没有这个头，判不出来是正常现象。
func IsJPEGHeader(data []byte) bool {
	return len(data) >= 3 &&
		data[0] == 0xFF &&
		data[1] == 0xD8 &&
		data[2] == 0xFF
}

// Feature 是一簇数据的紧凑摘要。
//
// 之所以要紧凑：整卡空闲簇上百万，要同时建索引，原始簇数据一律不留。
//
// 字段分三组，各管一个问题，别混着用：
//
//  1. 像不像 JPEG：FFCount、StuffedCount、RestartCount、Entropy
//  2. JPEG 的结构：SOIOffset、EOIOffset、重启标记那几个
//  3. 簇边界：TailFF、HeadMarker
//
// 所有偏移都是相对本簇开头的。
type Feature struct {
	// 本簇的字节数。
	BlockSize int

	// 熵编码流的统计量。
	FFCount      int
	StuffedCount int // FF 00 填充：熵编码里的 0xFF 后面必须补一个 0x00
	RestartCount int // FF D0 ~ FF D7 重启标记的个数
	Entropy      float64

	// 簇内找到的 JPEG 标记位置，-1 表示这一簇里没有。
	SOIOffset int // FF D8 起始标记
	EOIOffset int // FF D9 结束标记

	// 重启标记信息。
	//
	// FirstRST / LastRST 是重启编号 0~7，正常按下面这样循环递增：
	//
	//	RST0 -> RST1 -> …… -> RST7 -> RST0
	//
	FirstRST int8
	LastRST  int8

	// 首个和末个重启标记在簇内的绝对偏移。
	//
	// 有了它，不必保留簇内容也能拿「A 的尾巴」和「B 的头」比距离。
	FirstRSTOffset int
	LastRSTOffset  int

	// 簇内头两个重启标记之间的字节距离。
	//
	// 0 表示这一簇里的重启标记不足两个，量不出来。
	RSTGap int

	// 簇尾是一个孤立的 0xFF：标记或填充被簇边界切开了，
	// 它的后半个字节在下一簇头上，可能补成 FF 00，也可能补成某个标记。
	TailFF bool

	// 簇首是 0xFF 且后面不是 0x00：说明这一簇正好从一个标记边界开始，
	// 而不是接在上半个字节后面。
	HeadMarker bool
}

// Scan 一趟扫完，把这一簇里有用的 JPEG 特征全提出来。
//
// 调用方通常是这样用的：读一簇到临时缓冲 → Scan(data) → 丢掉 data。
// 所以扫描过程不需要整张盘常驻内存。
func Scan(data []byte) Feature {
	f := Feature{
		BlockSize:      len(data),
		SOIOffset:      -1,
		EOIOffset:      -1,
		FirstRST:       -1,
		LastRST:        -1,
		FirstRSTOffset: -1,
		LastRSTOffset:  -1,
	}

	if len(data) == 0 {
		return f
	}

	var freq [256]int

	// 上一个重启标记的偏移，用来算相邻间距。
	prevRSTOffset := -1

	for i := 0; i < len(data); i++ {
		b := data[i]
		freq[b]++

		if b != 0xFF {
			continue
		}

		f.FFCount++

		// 0xFF 是本簇最后一个字节，它的后半个字节在下一簇，看不见就不猜。
		if i+1 >= len(data) {
			continue
		}

		next := data[i+1]

		switch {
		case next == 0x00:
			// 熵编码数据里的 0xFF 一律写成 FF 00。
			f.StuffedCount++

		case next == 0xD8:
			// 起始标记 SOI。
			// 空隙里的不算：EOI 之后躺着的是别的文件残骸，那里的 FF D8 跟本文件无关。
			if f.SOIOffset < 0 && f.EOIOffset < 0 {
				f.SOIOffset = i
			}

		case next == 0xD9:
			// 结束标记 EOI。只认第一个：之后的字节已经不是这个流了。
			if f.EOIOffset < 0 {
				f.EOIOffset = i
			}

		case next >= 0xD0 && next <= 0xD7:
			// 重启标记 RSTn。
			//
			// 整段都在「流还没结束」的前提下才记：EOI 之后的空隙里同样能扫出
			// FF D0~D7（上一个文件的数据），那是别人的节奏，混进来会把 RSTGap
			// 和编号全带偏，打分就跟着错。
			if f.EOIOffset < 0 {
				phase := int8(next - 0xD0)

				if f.FirstRST < 0 {
					f.FirstRST = phase
					f.FirstRSTOffset = i
				}

				if prevRSTOffset >= 0 && f.RSTGap == 0 {
					// 只取第一对间距当这一簇的代表间距：有节奏的流每对都一样。
					f.RSTGap = i - prevRSTOffset
				}

				f.LastRST = phase
				f.LastRSTOffset = i
				f.RestartCount++

				prevRSTOffset = i
			}
		}
	}

	// 簇边界状态：能不能和邻簇咬合，就看这两位。
	f.TailFF = data[len(data)-1] == 0xFF

	f.HeadMarker =
		data[0] == 0xFF &&
			(len(data) < 2 || data[1] != 0x00)

	f.Entropy = shannonEntropy(freq[:], len(data))

	return f
}

// HasRSTRhythm 判断这一簇里的重启标记够不够多，能不能据此估出内部的重启间隔。
func HasRSTRhythm(f Feature) bool {
	return f.RestartCount >= 2 && f.RSTGap > 0
}

// IsLikelyJPEG 给「非文件头的碎片」做宽松的候选筛选。
//
// 它刻意不是 JPEG 校验器 —— 只负责把候选挑出来，后续再做碎片分析。
//
// 主信号是 FF 00 填充的密度。随机压缩数据也会偶尔出现 FF 00，所以只能当筛选。
//
// 阈值取得刻意宽松，不依赖重启标记：很多编码器压根不开重启间隔，
// 要求有重启标记才能入选会漏掉绝大多数照片。
func IsLikelyJPEG(f Feature) bool {
	if f.BlockSize == 0 {
		return false
	}

	return f.StuffedCount >= 16 &&
		float64(f.StuffedCount)/float64(f.BlockSize) >= 1.0/2048.0
}

// IsJPEGStart 判断这一簇里有没有出现 JPEG 起始标记 SOI。
// 用来认「这是不是某个文件的第一簇」。
func IsJPEGStart(f Feature) bool {
	return f.SOIOffset >= 0
}

// IsJPEGEnd 判断这一簇里有没有出现 JPEG 结束标记 EOI。
// 用来认「这是不是某个文件的最后一簇」。
func IsJPEGEnd(f Feature) bool {
	return f.EOIOffset >= 0
}

// RSTPhaseNext 判断 b 的首个重启标记，是不是正好接在 a 的末个重启标记后面。
//
// 重启标记按下面这样循环递增：
//
//	RST0 -> RST1 -> …… -> RST7 -> RST0
//
// 两边只要有一边没有可用的重启标记信息，就返回 false（没有信息不等于接对了）。
func RSTPhaseNext(a, b Feature) bool {
	if a.LastRST < 0 || b.FirstRST < 0 {
		return false
	}

	expected := (a.LastRST + 1) & 7
	return b.FirstRST == expected
}

// RSTGapRatio 比较两簇各自的重启间隔有多接近。
//
// 返回值：
//
//   - 信息不足：0
//   - 两个间隔一样：1
//   - 差得越远，越接近 0
func RSTGapRatio(a, b Feature) float64 {
	if a.RSTGap <= 0 || b.RSTGap <= 0 {
		return 0
	}

	x := float64(a.RSTGap)
	y := float64(b.RSTGap)

	minGap := math.Min(x, y)
	maxGap := math.Max(x, y)

	if maxGap == 0 {
		return 0
	}

	return minGap / maxGap
}

// StuffedDensity 返回这一簇里 FF 00 的密度。
func StuffedDensity(f Feature) float64 {
	if f.BlockSize <= 0 {
		return 0
	}

	return float64(f.StuffedCount) / float64(f.BlockSize)
}

// FFDensity 单独留一份 0xFF 的密度，不跟 StuffedDensity 混：
// 它更弱，但有时候能当个兜底信号。
func FFDensity(f Feature) float64 {
	if f.BlockSize <= 0 {
		return 0
	}

	return float64(f.FFCount) / float64(f.BlockSize)
}

// ScoreNext 估计 b 紧跟在 a 后面的可能性有多大。
//
// 分数是有方向的：ScoreNext(a, b) 问的是「A → B 有多说得通」。
//
// 它刻意不返回布尔值。碎片恢复本来就是不确定的事，几条弱证据合起来打分，
// 比硬判「是/否」更贴近现实。
//
// 大致这么看：
//
//	>= 8   很强的候选
//	5~8    不错的候选
//	2~5    有可能
//	< 2    偏弱
//	< 0    明显冲突
//
// 这些阈值只是起点，得拿真恢复出来的 JPEG 对过再调。
func ScoreNext(a, b Feature) float64 {
	if a.BlockSize == 0 || b.BlockSize == 0 {
		return -math.MaxFloat64
	}

	score := 0.0

	// ------------------------------------------------------------
	// 一、结构性冲突（最接近硬判据的部分）
	// ------------------------------------------------------------

	// A 里有 EOI：这个 JPEG 到它为止，后面不该再接普通碎片。
	if a.EOIOffset >= 0 {
		return -100
	}

	// B 以 SOI 开头：它本身是另一个文件的第一簇，是硬冲突，跟「A 里有 EOI」一样判死。
	if b.SOIOffset == 0 {
		return -100
	}

	// B 里有 SOI 但不在开头：说明本文件在 B 中间就结束了，后面是空隙，
	// 跟「A → B 相不相邻」没关系，不扣分。
	//
	// 这一条之前写成了「只要有 SOI 就扣 20 分」，于是所有最后一簇都被判成冲突 ——
	// 它们的尾部空隙里常躺着上一个文件的 SOI。实测一对真正相邻的簇被打成 -17 分。
	// 注意 Scan 那边也做了处理：EOI 之后的标记一律不认。

	// ------------------------------------------------------------
	// 二、簇边界上的证据
	// ------------------------------------------------------------

	// A 以 0xFF 结尾：说明那个字节的后半在 B 里。最强的情况是
	//
	//	A: …… FF
	//	B: 00 ……
	//
	// 这里刻意不去校验 B[0]，因为 Feature 没保留原始首字节。
	// 等这条边成为高置信候选时，再回读原始字节核对。
	if a.TailFF {
		score += 0.5
	}

	// B 从一个标记边界开始：弱正向证据，说明它可能正好起于一个有意义的边界。
	if b.HeadMarker {
		score += 0.25
	}

	// ------------------------------------------------------------
	// 三、重启标记的编号连续性
	// ------------------------------------------------------------

	if a.LastRST >= 0 && b.FirstRST >= 0 {
		if RSTPhaseNext(a, b) {
			score += 4.0
		} else {
			// 编号接不上是实打实的反向证据。
			score -= 3.0
		}
	}

	// ------------------------------------------------------------
	// 四、重启间隔的相似度
	// ------------------------------------------------------------

	if a.RSTGap > 0 && b.RSTGap > 0 {
		ratio := RSTGapRatio(a, b)

		switch {
		case ratio >= 0.95:
			score += 3.0
		case ratio >= 0.80:
			score += 2.0
		case ratio >= 0.60:
			score += 0.75
		default:
			score -= 1.5
		}
	}

	// ------------------------------------------------------------
	// 五、熵编码流的相似度
	// ------------------------------------------------------------

	// FF 00 密度接近有点用，但刻意给得很弱：
	// 两个毫不相干的压缩流也可能有相近的统计特征。
	da := StuffedDensity(a)
	db := StuffedDensity(b)

	if da > 0 && db > 0 {
		ratio := math.Min(da, db) / math.Max(da, db)

		switch {
		case ratio >= 0.80:
			score += 1.0
		case ratio >= 0.60:
			score += 0.5
		}
	}

	// 熵同样只是弱证据。别要求相等：同一张图不同区域的局部内容差别可以很大。
	if a.Entropy > 0 && b.Entropy > 0 {
		diff := math.Abs(a.Entropy - b.Entropy)

		switch {
		case diff < 0.20:
			score += 0.5
		case diff < 0.50:
			score += 0.25
		}
	}

	return score
}

// HasStrongRSTEvidence 判断 A → B 是否同时满足「重启编号连得上」和「重启间隔对得上」，
// 两条都成立才算强证据。
func HasStrongRSTEvidence(a, b Feature) bool {
	return a.LastRST >= 0 &&
		b.FirstRST >= 0 &&
		RSTPhaseNext(a, b) &&
		RSTGapRatio(a, b) >= 0.80
}

// shannonEntropy 按字节频数算香农熵（bit/字节），只用来给相似度打分当参考。
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
