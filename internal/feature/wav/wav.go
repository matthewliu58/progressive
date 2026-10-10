// Package wav 判断一簇数据像不像 WAV，并给「两簇是否相邻」打分，供文件雕刻使用。
//
// 设计前提跟 jpeg 包一样：
//   - 不保留原始簇数据。整卡空闲簇上百万，留在内存里就是几十 GB；
//   - Scan 从一簇里提取出紧凑的 Feature，随即可以丢掉原始数据；
//   - IsLikelyWAV 只做宽松的候选筛选，不是 WAV 校验器；
//   - ScoreNext(a, b) 估计「b 紧跟在 a 后面」的可能性有多大。
//
// 这个包不能证明两簇属于同一个 WAV —— 采样流（尤其 PCM）里任意字节都能接任意字节，
// 它只能给上层的雕刻算法提供证据（能用来否决，不能用来确认）。真正「接对了」只有
// verify.go 把整份 RIFF 结构走通才算数。
//
// 和 png 包的关系：WAV 跟 PNG 一样是分块结构，所以判据骨架（按 chunk 走通 + 簇边界
// 咬合）直接仿 PNG。但三处不一样，别照搬：
//   - WAV 的 chunk 长度是**小端**（PNG 是大端）；
//   - WAV 的 chunk ID 允许空格（"fmt "、"data"），PNG 只认字母；
//   - WAV 没有 CRC、也**没有结束标记** —— 文件到哪算完靠目录项的 DataLength，
//     跟 MP4 / MOV 一样，所以 IsWAVEnd 恒为 false。
package wav

import (
	"bytes"
	"encoding/binary"
	"math"
)

// RIFF / WAVE 标记。WAV 是 RIFF 的一种：开头 "RIFF" + 4 字节小端长度 + "WAVE"，
// 后面跟一串 chunk。靠偏移 8 处的 "WAVE" 能把 WAV 跟同样用 RIFF 的 AVI / WEBP 分开。
var (
	riffMagic = []byte("RIFF")
	waveMagic = []byte("WAVE")
)

const (
	// chunkHeader 是一个 RIFF chunk 的头长：4 字节 ID + 4 字节小端长度。
	chunkHeader = 8

	// riffHeader 是 "RIFF" + 长度 + "WAVE" 的总长。
	riffHeader = 12

	// maxChunkLength 是单个 chunk 数据段的最大长度（uint32 上限）。
	maxChunkLength = (1 << 32) - 1
)

// 簇尾状态：A 这一簇的最后一个字节落在 chunk 结构的什么位置。见 Tail* 常量。
const (
	TailUnknown int8 = iota // 走不通了，或者簇太小，没法判断
	TailClean               // 正好停在一个完整 chunk 的末尾（padding 之后）：B 应从新 chunk 起
	TailInData              // 停在某个 chunk 的 data 中间：B 应续上该 chunk 的 data
	TailInID                // 停在 chunk 的 ID 字段残片里（length 读全、ID 只读了一部分）
	TailInLen               // 停在 chunk 的 length 字段残片里（只读到了 1~3 字节 length）
)

// 簇首状态（镜像）：B 这一簇的第一个字节落在 chunk 结构的什么位置。
const (
	HeadUnknown int8 = iota
	HeadClean        // 簇首正好是一个 chunk 的起点（4 字节 ID + 4 字节 length 都读得到且合法）
	HeadInData       // 簇首是某个 chunk 的 data 中间（或干脆走不通）：是「续上来的数据」
)

// Feature 是一簇数据的紧凑摘要。
//
// 字段分三组，各管一个问题，别混着用：
//
//  1. 像不像 WAV：HasWave、ChunkCount、ChunkWalkEnd
//  2. WAV 的结构：HasFmt、HasData、HasLIST
//  3. 簇边界：TailState、HeadState、OpenChunkID、OpenChunkRemain
//
// 所有偏移都是相对本簇开头的。
type Feature struct {
	// 本簇的字节数。
	BlockSize int

	// 簇首就是 "RIFF" + "WAVE" 签名。只在第一簇为真；后续碎片本来就没有签名，正常。
	HasWave bool

	// 从某个起点开始，按 chunk 结构（ID + length + data，长度小端、偶数对齐）能连续
	// 走通多少个字节。这是「像不像 WAV」最强的单项证据：随机数据几乎不可能一直走通。
	ChunkWalkEnd int

	// 这一簇里成功解析出的 chunk 个数（跨边界没走完的不算）。
	ChunkCount int

	// 簇内找到的关键 chunk 类型。
	HasFmt  bool // 格式块：每个 WAV 必须有，里面是采样率/声道/位深
	HasData bool // 数据块：PCM / 压缩后的采样流，通常占文件绝大部分
	HasLIST bool // LIST 块：可选元数据（INFO 等）

	// 簇尾落在 chunk 结构的什么位置，见 Tail* 常量。
	TailState int8

	// 簇首落在 chunk 结构的什么位置，见 Head* 常量。
	HeadState int8

	// 当前被簇边界切开的那个未完成 chunk 的信息。
	//
	// 如果簇尾停在某个 chunk 的 data 中间（TailInData），记下来它的 ID 和还剩多少
	// 字节没读完，便于邻簇咬合时核对（不过真要核对还是回读原始字节更稳）。
	OpenChunkID     [4]byte
	OpenChunkRemain int
}

// IsWAVHeader 判断 data 是不是以 WAV 的 RIFF 头开头，即：
//
//	52 49 46 46  ?? ?? ?? ??  57 41 56 45   ("RIFF" ... "WAVE")
//
// 只适用于文件的第一簇。后续碎片本来就没有这个头，判不出来是正常现象。
func IsWAVHeader(data []byte) bool {
	return len(data) >= riffHeader &&
		bytes.Equal(data[0:4], riffMagic) &&
		bytes.Equal(data[8:12], waveMagic)
}

// isValidChunkID 判断 4 字节是不是合法的 RIFF chunk ID。
//
// RIFF 规定 ID 是四字符码，通常是大写 ASCII 字母/数字，但 fmt、data、LIST 这类都带
// 尾随空格，所以放宽到「全部是可打印 ASCII」（0x20~0x7E）。这个判据很弱，必须配合
// 「长度合理 + 能一直走通」一起用。
func isValidChunkID(id []byte) bool {
	if len(id) != 4 {
		return false
	}
	for _, b := range id {
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	return true
}

// peekChunkHeader 读 data[pos:pos+8] 处的 chunk 头，返回数据段长度（小端）并判断是否合法。
func peekChunkHeader(data []byte, pos int) (int, bool) {
	if pos+chunkHeader > len(data) {
		return 0, false
	}
	if !isValidChunkID(data[pos : pos+4]) {
		return 0, false
	}
	length := binary.LittleEndian.Uint32(data[pos+4 : pos+8])
	if length > maxChunkLength {
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
		return TailInLen // 只读到了 1~3 字节 ID
	case rem < 8:
		return TailInID // ID 读全，length 只读到一部分
	default:
		return TailUnknown
	}
}

// Scan 一趟扫完，把这一簇里有用的 WAV 特征全提出来。
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
	if IsWAVHeader(data) {
		f.HasWave = true
		start = riffHeader
	}
	f.ChunkWalkEnd = start

	// 簇首状态：从 start 看是不是正好落在一个 chunk 的起点。
	if len(data) >= start+chunkHeader {
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
		if pos+chunkHeader > len(data) {
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

		id := [4]byte{data[pos], data[pos+1], data[pos+2], data[pos+3]}
		switch string(id[:]) {
		case "fmt ":
			f.HasFmt = true
		case "data":
			f.HasData = true
		case "LIST":
			f.HasLIST = true
		}
		f.ChunkCount++

		// chunk 总长 = 8 + length；RIFF 规定奇数长度要补 1 字节 padding 到偶数。
		end := pos + chunkHeader + length
		if end < pos { // 溢出防护
			f.TailState = TailUnknown
			return f
		}
		if length%2 == 1 {
			end++
		}

		if end > len(data) {
			// chunk 跨到下一簇：记下来这个未完成 chunk 的剩余字节。
			f.OpenChunkID = id
			f.OpenChunkRemain = end - len(data)
			f.TailState = TailInData
			f.ChunkWalkEnd = pos
			return f
		}

		pos = end
		f.ChunkWalkEnd = pos
	}
}

// IsLikelyWAV 给「非文件头的碎片」做宽松的候选筛选。
//
// 它刻意不是 WAV 校验器 —— 只负责把候选挑出来，后续再做碎片分析。
//
// 已知的根本局限（和 png 包注释里说的一模一样）：一旦落在 "data" 块中间，孤立的一簇
// 就是 PCM 采样（或压缩流），既没有签名、也没有 chunk 边界，统计上根本认不出来。
// 所以这里只认「从簇首开始就能连续走通至少 2 个合法 chunk、且走通的部分占满大半簇」
// 的情况——也就是正好落在 chunk 边界上的那些碎片。
//
// 落在采样流中间的大多数碎片靠这个判据会漏掉；它们真正的身份只能靠「从文件头顺着走
// 分块结构」+ verify.go 的结构校验来确认，而不是靠孤立聚类。
func IsLikelyWAV(f Feature) bool {
	if f.BlockSize == 0 {
		return false
	}

	// 带签名的文件头簇不走这里：那是 IsWAVHeader 的事。
	if f.HasWave {
		return true
	}

	ratio := float64(f.ChunkWalkEnd) / float64(f.BlockSize)
	return f.ChunkCount >= 2 && ratio >= 0.8
}

// IsWAVStart 判断这一簇里有没有出现 WAV 文件头签名。
// 用来认「这是不是某个文件的第一簇」。
func IsWAVStart(f Feature) bool {
	return f.HasWave
}

// IsWAVHead 判断这一簇是不是正好从签名开始（即它就是某个 WAV 的第一簇）。
// 签名只可能出现在偏移 0，所以和 IsWAVStart 在这里等价，仅为 API 对称保留。
func IsWAVHead(f Feature) bool {
	return f.HasWave
}

// IsWAVEnd 判断这一簇是不是文件的最后一簇。
//
// WAV 没有结束标记（不像 JPEG 的 EOI / PNG 的 IEND）："data" 块后面就是文件尾，靠
// 目录项的 DataLength 判断喂没喂饱。所以这里恒为 false。
func IsWAVEnd(f Feature) bool {
	return false
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
// 这些阈值只是起点，得拿真恢复出来的 WAV 对过再调。
func ScoreNext(a, b Feature) float64 {
	if a.BlockSize == 0 || b.BlockSize == 0 {
		return -math.MaxFloat64
	}

	score := 0.0

	// ------------------------------------------------------------
	// 一、结构性冲突
	// ------------------------------------------------------------

	// B 以签名开头：它本身是另一个 WAV 文件的第一簇，是硬冲突。
	// （A 这边没有「结束标记」可判：WAV 靠 DataLength，不靠流内标记。）
	if b.HasWave {
		return -100
	}

	// ------------------------------------------------------------
	// 二、簇边界上的证据：A 的尾巴 和 B 的头 是否咬合
	// ------------------------------------------------------------
	//
	// 和 PNG 一样：PNG 没有 JPEG 那种周期性锚点，但分块结构要求「A 停在边界则 B 必须
	// 起于新 chunk 头；A 停在 data 中间则 B 必须是续来的数据」，对不上就是实打实的冲突。

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
	// 三、chunk 走通深度：越深越像真的在顺着同一份 WAV 结构走
	// ------------------------------------------------------------

	if a.ChunkWalkEnd > start0(a) && b.ChunkWalkEnd > start0(b) {
		score += 0.5
	}

	return score
}

// start0 返回某个特征「应当开始走 chunk」的偏移：带签名是 12，否则是 0。
// 用来判断 ChunkWalkEnd 是不是真的走通了（而不是停在起点）。
func start0(f Feature) int {
	if f.HasWave {
		return riffHeader
	}
	return 0
}
