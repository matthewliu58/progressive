// Package png 判断一簇数据像不像 PNG，并给「两簇是否相邻」打分，供文件雕刻使用。
//
// 设计前提跟 jpeg 包一样：
//   - 不保留原始簇数据。整卡空闲簇上百万，留在内存里就是几十 GB；
//   - Scan 从一簇里提取出紧凑的 Feature，随即可以丢掉原始数据；
//   - IsLikelyPNG 只做宽松的候选筛选，不是 PNG 校验器；
//   - ScoreNext(a, b) 估计「b 紧跟在 a 后面」的可能性有多大。
//
// 这个包不能证明两簇属于同一个 PNG —— 分块结构里任意合法字节都能接任意合法字节，
// 它只能给上层的雕刻算法提供证据（能用来否决，不能用来确认）。真正「接对了」只有
// image/png.Decode 解到 IEND 才算数（见 verify.go）。
package png

import (
	"bytes"
	"encoding/binary"
	"math"
)

// pngSignature 是 PNG 的 8 字节文件签名。只适用于文件的第一簇。
var pngSignature = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

// maxChunkLength 是单个 chunk 数据段的最大长度（PNG 规范上限 2^31-1）。
// 超过它的「长度字段」一定是走飞了，不当成合法 chunk。
const maxChunkLength = (1 << 31) - 1

// 簇尾状态：A 这一簇的最后一个字节落在 chunk 结构的什么位置。
//
// 有了它，不必保留簇内容也能拿「A 的尾巴」和「B 的头」比咬合。
const (
	TailUnknown int8 = iota // 走不通了，或者簇太小，没法判断
	TailClean               // 正好停在一个完整 chunk 的末尾（CRC 之后）：B 应从新 chunk 起
	TailInData              // 停在某个 chunk 的 data 中间：B 应续上该 chunk 的 data
	TailInCRC               // data 正好读完，4 字节 CRC 被簇边界切开到 B 里
	TailInType              // 停在 chunk 的 type 字段残片里（length 读全、type 只读了一部分）
	TailInLen               // 停在 chunk 的 length 字段残片里（只读到了 1~3 字节 length）
)

// 簇首状态（镜像）：B 这一簇的第一个字节落在 chunk 结构的什么位置。
const (
	HeadUnknown int8 = iota
	HeadClean        // 簇首正好是一个 chunk 的起点（4 字节 length + 4 字节 type 都读得到且合法）
	HeadInData       // 簇首是某个 chunk 的 data 中间（或干脆走不通）：是「续上来的数据」
)

// Feature 是一簇数据的紧凑摘要。
//
// 字段分三组，各管一个问题，别混着用：
//
//  1. 像不像 PNG：HasSignature、ChunkCount、ChunkWalkEnd
//  2. PNG 的结构：HasIHDR、HasIDAT、HasIEND
//  3. 簇边界：TailState、HeadState、OpenChunkType、OpenChunkRemain
//
// 所有偏移都是相对本簇开头的。
type Feature struct {
	// 本簇的字节数。
	BlockSize int

	// 簇首就是 PNG 的 8 字节签名。只在第一簇为真；后续碎片本来就没有签名，正常。
	HasSignature bool

	// 从某个起点开始，按 chunk 结构（length + type + data + crc）能连续走通多少个字节。
	//
	// 这是「像不像 PNG」最强的单项证据：随机数据几乎不可能一直走通。
	ChunkWalkEnd int

	// 这一簇里成功解析出的 chunk 个数（跨边界没走完的不算）。
	ChunkCount int

	// 簇内找到的关键 chunk 类型。
	HasIHDR bool // 图像头：理论上每个 PNG 第一个 chunk 必是它
	HasIDAT bool // 图像数据：zlib/deflate 压缩流，通常占文件绝大部分
	HasIEND bool // 图像尾：IEND 之后文件就该结束了

	// 簇尾落在 chunk 结构的什么位置，见 Tail* 常量。
	TailState int8

	// 簇首落在 chunk 结构的什么位置，见 Head* 常量。
	HeadState int8

	// 当前被簇边界切开的那个未完成 chunk 的信息。
	//
	// 如果簇尾停在某个 chunk 的 data 中间（TailInData），记下来它的类型和还剩多少
	// 字节没读完，便于邻簇咬合时核对（不过真要核对还是回读原始字节更稳）。
	OpenChunkType   [4]byte
	OpenChunkRemain int
}

// IsPNGHeader 判断 data 是不是以 PNG 的签名开头，即：
//
//	89 50 4E 47 0D 0A 1A 0A
//
// 只适用于文件的第一簇。后续碎片本来就没有这个头，判不出来是正常现象。
func IsPNGHeader(data []byte) bool {
	return len(data) >= len(pngSignature) && bytes.Equal(data[:len(pngSignature)], pngSignature)
}

// isValidChunkType 判断 4 字节是不是合法的 PNG chunk 类型。
//
// PNG 规定类型码四个字节都必须是 ASCII 字母（A-Z / a-z），大小写另有语义但都是字母。
// 这个判据很弱（随机四字母碰巧也过），所以必须配合「长度合理 + 能一直走通」一起用。
func isValidChunkType(typ []byte) bool {
	if len(typ) != 4 {
		return false
	}
	for _, b := range typ {
		if !((b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')) {
			return false
		}
	}
	return true
}

// peekChunkHeader 读 data[pos:pos+8] 处的 chunk 头，返回数据段长度并判断是否合法。
func peekChunkHeader(data []byte, pos int) (int, bool) {
	if pos+8 > len(data) {
		return 0, false
	}
	length := binary.BigEndian.Uint32(data[pos : pos+4])
	if length > maxChunkLength {
		return 0, false
	}
	if !isValidChunkType(data[pos+4 : pos+8]) {
		return 0, false
	}
	return int(length), true
}

// tailStateForHeaderFragment 判断「下一个 chunk 头读到一半」时簇尾处于哪种残片状态。
func tailStateForHeaderFragment(data []byte, pos int) int8 {
	rem := len(data) - pos
	switch {
	case rem == 0:
		return TailClean // 恰好走到簇尾且一个 chunk 头都不缺：视为停在边界上
	case rem < 4:
		return TailInLen // 只读到了 1~3 字节 length
	case rem < 8:
		return TailInType // length 读全，type 只读到一部分
	default:
		return TailUnknown
	}
}

// Scan 一趟扫完，把这一簇里有用的 PNG 特征全提出来。
//
// 调用方通常是这样用的：读一簇到临时缓冲 → Scan(data) → 丢掉 data。
// 所以扫描过程不需要整张盘常驻内存。
func Scan(data []byte) Feature {
	f := Feature{
		BlockSize:    len(data),
		TailState:    TailUnknown,
		HeadState:    HeadUnknown,
		ChunkWalkEnd: 0,
	}

	if len(data) == 0 {
		return f
	}

	// 签名只在簇首有意义；后续碎片不带签名，从偏移 0 直接按 chunk 走。
	start := 0
	if IsPNGHeader(data) {
		f.HasSignature = true
		start = len(pngSignature)
	}
	f.ChunkWalkEnd = start

	// 簇首状态：从 start 看是不是正好落在一个 chunk 的起点。
	if len(data) >= start+8 {
		if _, ok := peekChunkHeader(data, start); ok {
			f.HeadState = HeadClean
		} else {
			f.HeadState = HeadInData
		}
	} else {
		f.HeadState = HeadInData
	}

	pos := start
	for {
		if pos+8 > len(data) {
			// 读不全下一个 chunk 头，按残片状态收尾。
			f.TailState = tailStateForHeaderFragment(data, pos)
			return f
		}

		length, ok := peekChunkHeader(data, pos)
		if !ok {
			// 走到非法 chunk 头：要么数据错了，要么是别的文件残骸，走飞了。
			f.TailState = TailUnknown
			return f
		}

		typ := [4]byte{data[pos+4], data[pos+5], data[pos+6], data[pos+7]}
		switch string(typ[:]) {
		case "IHDR":
			f.HasIHDR = true
		case "IDAT":
			f.HasIDAT = true
		case "IEND":
			f.HasIEND = true
		}
		f.ChunkCount++

		// chunk 总长 = 12 + length（length(4) + type(4) + data(length) + crc(4)）。
		end := pos + 12 + length
		if end < pos { // 溢出防护：length 接近上限时 int 相加可能回绕
			f.TailState = TailUnknown
			return f
		}

		if end > len(data) {
			// chunk 跨到下一簇：记下来这个未完成 chunk 的剩余字节。
			f.OpenChunkType = typ
			f.OpenChunkRemain = end - len(data)
			if pos+8+length == len(data) {
				f.TailState = TailInCRC // data 正好读完，crc 在下一簇
			} else {
				f.TailState = TailInData
			}
			f.ChunkWalkEnd = pos
			return f
		}

		pos = end
		f.ChunkWalkEnd = pos

		if f.HasIEND {
			// IEND 之后不该再有数据；后面躺着的是别的文件残骸，那里走通的 chunk 不算数。
			break
		}
	}

	if pos == len(data) {
		f.TailState = TailClean
	} else {
		f.TailState = TailUnknown
	}
	return f
}

// IsLikelyPNG 给「非文件头的碎片」做宽松的候选筛选。
//
// 它刻意不是 PNG 校验器 —— 只负责把候选挑出来，后续再做碎片分析。
//
// 已知的根本局限（和 MP4 注释里说的一模一样）：一旦落在 IDAT 的 zlib/deflate 压缩流
// 中间，孤立的一簇就是压缩过的「看起来像随机」的字节，既没有签名、也没有 chunk 边界，
// 统计上根本认不出来。所以这里只认「从簇首开始就能连续走通至少 2 个合法 chunk、且走通
// 的部分占满大半簇」的情况——也就是正好落在 chunk 边界上的那些碎片。
//
// 落在压缩流中间的大多数碎片靠这个判据会漏掉；它们真正的身份只能靠「从文件头顺着走
// 分块结构」+ verify.go 的真解码来确认，而不是靠孤立聚类。
func IsLikelyPNG(f Feature) bool {
	if f.BlockSize == 0 {
		return false
	}

	// 带签名的文件头簇不走这里：那是 IsPNGHeader 的事。
	if f.HasSignature {
		return true
	}

	ratio := float64(f.ChunkWalkEnd) / float64(f.BlockSize)
	return f.ChunkCount >= 2 && ratio >= 0.8
}

// IsPNGStart 判断这一簇里有没有出现 PNG 文件头签名。
// 用来认「这是不是某个文件的第一簇」。
func IsPNGStart(f Feature) bool {
	return f.HasSignature
}

// IsPNGHead 判断这一簇是不是正好从签名开始（即它就是某个 PNG 的第一簇）。
// 签名只可能出现在偏移 0，所以和 IsPNGStart 在这里等价，仅为 API 对称保留。
func IsPNGHead(f Feature) bool {
	return f.HasSignature
}

// IsPNGEnd 判断这一簇里有没有出现 IEND：文件数据流到此为止，后面不能再接。
func IsPNGEnd(f Feature) bool {
	return f.HasIEND
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
//	>= 6   很强的候选
//	3~6    不错的候选
//	1~3    有可能
//	< 1    偏弱
//	< 0    明显冲突
//
// 这些阈值只是起点，得拿真恢复出来的 PNG 对过再调。
func ScoreNext(a, b Feature) float64 {
	if a.BlockSize == 0 || b.BlockSize == 0 {
		return -math.MaxFloat64
	}

	score := 0.0

	// ------------------------------------------------------------
	// 一、结构性冲突（最接近硬判据的部分）
	// ------------------------------------------------------------

	// A 里有 IEND：这个 PNG 到它为止，后面不该再接普通碎片。
	if a.HasIEND {
		return -100
	}

	// B 以签名开头：它本身是另一个文件的第一簇，是硬冲突。
	if b.HasSignature {
		return -100
	}

	// ------------------------------------------------------------
	// 二、簇边界上的证据：A 的尾巴 和 B 的头 是否咬合
	// ------------------------------------------------------------
	//
	// 这是 PNG 版「重启标记相位连续」的替代品——PNG 没有周期性锚点，
	// 但分块结构要求「A 停在边界则 B 必须起于新 chunk 头；A 停在 data 中间则 B
	// 必须是续来的数据」，对不上就是实打实的冲突。

	switch {
	case a.TailState == TailClean && b.HeadState == HeadClean:
		// A 停在 chunk 边界、B 起于新 chunk 头：完美咬合。
		score += 4.0
	case a.TailState == TailInData && b.HeadState == HeadInData:
		// 两边都落在 data 中间：A 的尾巴续到 B 的头，弱正向（没法验证内容）。
		score += 1.0
	case a.TailState == TailClean && b.HeadState == HeadInData:
		// A 停在边界、B 却从 data 中间起：对不上。
		score -= 4.0
	case a.TailState == TailInData && b.HeadState == HeadClean:
		// A 停在 data 中间、B 却从新 chunk 起：对不上。
		score -= 4.0
	}

	// ------------------------------------------------------------
	// 三、chunk 走通深度：越深越像真的在顺着同一份 PNG 结构走
	// ------------------------------------------------------------

	// 两边都从各自簇首成功走通过 chunk（哪怕只走了一小段），说明各自都像是
	// PNG 的一部分，给一点弱正向，避免把所有候选都打成 0。
	if a.ChunkWalkEnd > start0(a) && b.ChunkWalkEnd > start0(b) {
		score += 0.5
	}

	return score
}

// start0 返回某个特征「应当开始走 chunk」的偏移：带签名是 8，否则是 0。
// 用来判断 ChunkWalkEnd 是不是真的走通了（而不是停在起点）。
func start0(f Feature) int {
	if f.HasSignature {
		return len(pngSignature)
	}
	return 0
}
