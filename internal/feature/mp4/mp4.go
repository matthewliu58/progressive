// Package mp4 判断一簇数据像不像 MP4，并给「两簇是否相邻」打分，供文件雕刻使用。
//
// 跟 jpeg 包两头不一样，先说清楚，免得用错：
//   - 头好认：MP4 开头就是 box 结构，ftyp 四个字母是硬标记，比统计特征可靠得多。
//     更值钱的是 moov：里面记着每个 chunk 在文件里的偏移，拿到它就是精确地图，
//     不用像 JPEG 那样靠统计特征猜。
//   - 尾难认：MP4 没有 EOI 那样的结束标记；媒体数据（mdat）内部也没有同步结构，
//     H.264 的 NAL 只是「4 字节长度前缀 + NAL 头」串起来的一串，跟 JPEG 的熵编码
//     流一样，任意字节都能接任意字节。
//
// 所以这个包跟 jpeg 包一样：只能提供证据，不能证明两簇属于同一个文件。
package mp4

import (
	"bytes"
	"encoding/binary"
	"math"
)

const (
	// boxHeader 是一个 box 头的长度：4 字节长度 + 4 字节类型。
	boxHeader = 8

	// maxNALSize 是单个 NAL 的最大长度。真实 NAL（一帧或一个分片）远小于此，
	// 放宽到 1MB 只是为了不让随机四字节被当成长度时跳出天际。
	maxNALSize = 1 << 20

	// codecScanMax 是找编码四字符码时最多看多少字节。stsd 在 moov 里、离文件头不远，
	// 没必要整簇搜一遍。
	codecScanMax = 64 << 10

	// ftypMaxSize 是 ftyp 盒子的合理上限。它通常只有 20~40 字节（品牌列表），
	// 大到离谱说明这里不是文件头 —— 只认 ftyp 四个字母容易被随机数据蒙中。
	ftypMaxSize = 4096
)

// IsMP4Header 判断 data 是不是 MP4 文件的开头：最前面 4 字节是第一个 box 的长度
// （大端），紧接着 4 字节必须是 ftyp。
//
// 只认 ftyp、不认具体品牌：MP4 / MOV / M4A / HEIC 这一家子第一个盒子都是它。
func IsMP4Header(data []byte) bool {
	if len(data) < boxHeader {
		return false
	}
	size := binary.BigEndian.Uint32(data)
	if size < boxHeader || size > ftypMaxSize {
		return false
	}
	return string(data[4:8]) == "ftyp"
}

// Feature 是一簇数据的紧凑摘要。字段按用途分三组：box 结构、媒体数据结构、统计。
type Feature struct {
	BlockSize int

	// 顶层 box 在簇内的偏移，-1 表示这一簇里没有。
	//
	// 只有从文件开头顺下来才认得出 box，所以中间簇和最后一簇这三项基本都是 -1 ——
	// 它们整簇都是 mdat 里的媒体数据，没有 box 结构可言。
	FTYPOffset int // ftyp：文件类型盒子，文件头的标志
	MOOVOffset int // moov：索引，里面有每个 chunk 的偏移表
	MDATOffset int // mdat：媒体数据的起点

	// H.264 AVCC 的 NAL 结构：4 字节长度前缀 + 1 字节 NAL 头。
	// 这是 mdat 内部唯一抓得住的结构，用来判断「这一簇像不像媒体数据」。
	NALFrames   int     // 数出来的 NAL 个数
	NALRunMax   int     // 最长的一段「连得上」的 NAL：连成串比零散几个可信得多
	NALDensity  float64 // 每 KB 的 NAL 个数，给相邻簇的相似度打分用
	NALCoverage float64 // 最长那一串覆盖了本簇多少比例：见 IsLikelyMP4 的说明

	// 哪一种假设扫出来的（见 nalHypothesis）：长度前缀几个字节、是不是 HEVC。
	// 记下来是为了事后能看出这张卡上的视频到底是什么编码格式。
	LengthSize int  // 2 或 4；0 = 没连成串
	HEVC       bool // true = 按 HEVC 的 NAL 头格式连上的
}

// Scan 一趟扫完，把这一簇里能抓的 MP4 特征提出来。
//
// 两件事：从 0 开始顺一遍顶层 box（只对文件头那几簇有意义），再扫一遍 NAL 结构。
// 跟 jpeg 包一样，扫完原始数据就可以丢了。
func Scan(data []byte) Feature {
	f := Feature{
		BlockSize:  len(data),
		FTYPOffset: -1,
		MOOVOffset: -1,
		MDATOffset: -1,
	}
	if len(data) == 0 {
		return f
	}

	scanBoxes(data, &f)
	scanNAL(data, &f)

	if f.BlockSize > 0 {
		f.NALDensity = float64(f.NALFrames) / (float64(f.BlockSize) / 1024.0)
	}
	return f
}

// scanBoxes 从簇首开始顺一遍顶层 box，记下 ftyp / moov / mdat 的位置。
//
// 走到不合法的地方就停：中间簇整簇都是媒体数据，第一个「长度」就对不上，
// 这时不会误记任何 box（三个偏移保持 -1）。
// 撞上 mdat 也停：它后面就是媒体数据，不再是 box 的天下了。
func scanBoxes(data []byte, f *Feature) {
	off := 0
	for off+boxHeader <= len(data) {
		size := binary.BigEndian.Uint32(data[off:])
		typ := string(data[off+4 : off+8])

		// 长度不合法（比 box 头还小、或者超出这一簇）就当结构到头了。
		if size < boxHeader || uint64(off)+uint64(size) > uint64(len(data)) {
			return
		}
		switch typ {
		case "ftyp":
			if f.FTYPOffset < 0 {
				f.FTYPOffset = off
			}
		case "moov":
			if f.MOOVOffset < 0 {
				f.MOOVOffset = off
			}
		case "mdat":
			if f.MDATOffset < 0 {
				f.MDATOffset = off
			}
			return
		}
		off += int(size)
	}
}

// nalHypothesis 是一组「NAL 长什么样」的假设。
//
// 为什么要有假设：MP4 里的 NAL 是「长度前缀 + NAL 头」，但这两个都不是定死的 ——
//   - 长度前缀几个字节由 avcC 里的 NALUnitLengthSizeMinusOne 决定，常见 4，也有 2；
//   - H.264 和 HEVC 的 NAL 头里，类型位的位置不一样（HEVC 在 bit 1~6，还带层号）。
//
// 拍死一种就会整片漏判（实测正是如此：纯视频的簇一个都判不出来）。所以几种组合
// 都试，取连得最长的那一组 —— 真链在任何一种假设下都能连成长串，假链在四种假设
// 下都连不长。
type nalHypothesis struct {
	lengthSize int  // 长度前缀占几个字节
	hevc       bool // 是不是 HEVC 的 NAL 头格式
}

// 只用 4 字节这一种。
//
// 2 字节长度的假设试过，被实测打回来了：长度只有 16 位时，「长度 ≤ maxNALSize」
// 这条判据形同虚设 —— 任何 16 位值都必然通过，于是只剩 NAL 类型位可依（命中率
// 37.5%），链随手就能连起来。实测结果是一整张卡的簇有 99% 被判成 MP4 payload。
// 要支持 2 字节长度，得先找到别的硬判据（比如长度必须连起来正好铺满这一簇），
// 光靠类型位不够。
var nalHypotheses = []nalHypothesis{
	{lengthSize: 4, hevc: false}, // H.264：最常见，先试它
	{lengthSize: 4, hevc: true},  // HEVC：类型位位置不同
}

// nalLength 按假设读出 pos 处的长度前缀。
func nalLength(data []byte, pos, size int) uint32 {
	if size == 2 && pos+2 <= len(data) {
		return uint32(binary.BigEndian.Uint16(data[pos:]))
	}
	return binary.BigEndian.Uint32(data[pos:])
}

// nalTypeValid 判断 NAL 头的类型位是不是合法的那几种。
//
// H.264：类型在 bit 0~4，取 1~12。除了常见的 1 / 5（图像片）、6（SEI）、7（SPS）、
// 8（PPS），必须包含 **9（AUD，访问单元分隔符）** —— 手机录的视频常在每帧前面插
// 一个 AUD，漏掉它链会在每一帧那里断一次。0 是未指定、24~31 是保留和聚合类。
//
// HEVC：类型在 bit 1~6（`(b >> 1) & 0x3F`），取 1~40（VCL 与非 VCL 都算）。
//
// 放宽的代价可控：单字节命中概率最高到 40/64，但「连着三个都撞对」的概率仍在
// 1e-11 量级（见 IsLikelyMP4 的说明），误报不会因此抬头。
func nalTypeValid(nal byte, hevc bool) bool {
	if hevc {
		t := int(nal>>1) & 0x3F
		return t >= 1 && t <= 40
	}
	t := nal & 0x1F
	return t >= 1 && t <= 12
}

// nalLooksValid 判断 pos 处像不像一个 NAL 的开始：长度前缀合理，紧跟的 NAL 头类型对。
func nalLooksValid(data []byte, pos int, h nalHypothesis) bool {
	if pos < 0 || pos+h.lengthSize+1 > len(data) {
		return false
	}
	size := nalLength(data, pos, h.lengthSize)
	return size > 0 && size <= maxNALSize && nalTypeValid(data[pos+h.lengthSize], h.hevc)
}

// scanNAL 数一簇里能连成串的 NAL。
//
// 难点：起点无从知道。簇首通常落在某个 NAL 的中间，只能逐字节试。
//
// 关键在「往前看一步」。只判当前这一个是不够的：随机数据里「长度合理 + NAL 头类型对」
// 的概率约 4e-5，一簇 26 万个起始位置，撞出几十个假的不稀奇。一旦信了假的、顺着它的
// 长度跳出去，真正的 NAL 链就被跳过去了 —— 而且单趟扫描不会回头，整簇就废了。
// 所以这里要求「下一个也对得上」才收：假链几乎不可能连着两个都撞对（概率 1e-9），
// 真链则必然满足。
//
// 收尾补一个跨簇的 NAL：链尾之后若正好躺着一个 NAL 头（长度对、类型对，但尾巴在
// 下一簇），补记一个 —— 位置是上一步算出来的，不是猜的。
func scanNAL(data []byte, f *Feature) {
	// 几种假设各扫一遍，取连得最长的那一组。
	for _, h := range nalHypotheses {
		frames, runMax, lastEnd, tailRun, coverMax := scanNALBy(data, h)

		// 收尾补一个跨簇的 NAL：链尾之后若正好躺着一个 NAL 头（长度对、类型对，
		// 但尾巴在下一簇），补记一个 —— 位置是上一步算出来的，不是猜的。
		if lastEnd >= 0 && nalLooksValid(data, lastEnd, h) {
			frames++
			if tailRun+1 > runMax {
				runMax = tailRun + 1
			}
		}

		if runMax > f.NALRunMax {
			f.NALFrames, f.NALRunMax = frames, runMax
			f.LengthSize, f.HEVC = h.lengthSize, h.hevc
			if f.BlockSize > 0 {
				f.NALCoverage = float64(coverMax) / float64(f.BlockSize)
			}
		}
	}
}

// scanNALBy 按一种假设扫一遍，返回：数出的 NAL 个数、最长的一串、链尾位置、
// 以及链尾那一串的长度（收尾补记跨簇 NAL 时要用）。
func scanNALBy(data []byte, h nalHypothesis) (frames, runMax, lastEnd, tailRun, coverMax int) {
	run := 0
	covered := 0 // 当前这一串一共铺了多少字节
	lastEnd = -1

	for i := 0; i+h.lengthSize+1 <= len(data); {
		size := nalLength(data, i, h.lengthSize)
		if size == 0 || size > maxNALSize || !nalTypeValid(data[i+h.lengthSize], h.hevc) {
			run = 0
			covered = 0
			i++
			continue
		}

		next := i + h.lengthSize + int(size)
		take := func() {
			frames++
			run++
			covered += h.lengthSize + int(size)
			if run > runMax {
				runMax = run
				coverMax = covered
			}
			lastEnd, tailRun = next, run
			i = next
		}

		switch {
		case next > len(data):
			// 尾巴在下一簇：本轮不收（没法验证），留给收尾那一步。
			run = 0
			covered = 0
			i++
		case next+h.lengthSize+1 > len(data):
			// 本簇到此为止，后面没法再验证：收下这一个。
			take()
		case nalLooksValid(data, next, h):
			// 下一个 NAL 也对得上：收下，顺着跳。
			take()
		default:
			// 只有自己像、下一个不像 —— 多半是撞上的假货。
			run = 0
			covered = 0
			i++
		}
	}
	return
}

// IsLikelyMP4 给「非文件头的碎片」做宽松的候选筛选。
//
// 判据是 NAL 的连贯性：随机数据里，一个位置要同时满足「4 字节长度合理」和
// 「NAL 头类型对」的概率约 4e-5，连着三个都满足的概率在 1e-13 量级 —— 基本不可能是
// 巧合。真实的 mdat 是一串首尾相接的 NAL，链根本断不了。
//
// 阈值只要 3，不能再高了：簇 256KB 时，一个 4K 的 I 帧就能占掉大半簇，
// 一簇里可能只有两三个 NAL。原来要 4 个连续 + 8 个总数，实测直接漏掉整簇。
//
// 再加一条「覆盖率」：最长那一串必须铺满本簇至少 20%。真实的 mdat 是一串 NAL
// 首尾相接铺满整簇（除头尾各一小截），碰巧撞出来的短串则铺不了多远。这一条是
// 保险 —— 判据一旦被放宽（比如将来又加了新假设），它能兜住误判。
func IsLikelyMP4(f Feature) bool {
	return f.NALRunMax >= 3 && f.NALFrames >= 3 && f.NALCoverage >= 0.2
}

// IsMP4Start 判断这一簇里有没有出现 ftyp（文件头标志）。
func IsMP4Start(f Feature) bool {
	return f.FTYPOffset >= 0
}

// IsMP4Head 判断这一簇是不是 MP4 的第一簇：ftyp 就在簇首。
//
// 跟 IsMP4Start 的区别同 jpeg 包：判断目录项给的第一簇对不对，要用这个。
func IsMP4Head(f Feature) bool {
	return f.FTYPOffset == 0
}

// codecTags 是常见的采样项四字符码，出现在 moov 的 stsd 里。
var codecTags = []string{"avc1", "avc3", "hvc1", "hev1", "mp4a"}

// CodecTag 在数据里找采样项的四字符码，找不到返回空串。
//
// 为什么要单独探它：H.264（avc1）和 HEVC（hvc1/hev1）的 NAL 头长得不一样 ——
// 类型位的位置不同。识别器得先知道是哪种编码，才知道 NAL 该怎么认。
// 这条只是探测和日志用，不参与分类。
func CodecTag(data []byte) string {
	limit := len(data)
	if limit > codecScanMax {
		limit = codecScanMax
	}
	for _, tag := range codecTags {
		if bytes.Index(data[:limit], []byte(tag)) >= 0 {
			return tag
		}
	}
	return ""
}

// HasIndex 判断这一簇里有没有 moov。
//
// moov 里是 chunk 偏移表（stco / co64）：拿到它就能精确知道这个文件的每一段在
// 哪一簇，不用猜。所以它比什么都值钱，单独留个问法。
func HasIndex(f Feature) bool {
	return f.MOOVOffset >= 0
}

// ScoreNext 估计 b 紧跟在 a 后面的可能性有多大。
//
// 说在前面：MP4 的接缝证据比 JPEG 还弱。JPEG 至少有 FF 00 填充密度和重启标记，
// MP4 的 mdat 内部什么标记都没有，能用的只有「码率相近」这种弱统计，外加一条
// 硬否决（b 是另一个文件的头）。
//
// 所以别指望靠它排序 —— carve 那边现在是「距离优先、分数只否决」，正是这个原因。
func ScoreNext(a, b Feature) float64 {
	// b 以 ftyp 开头：它是另一个文件的第一簇，硬冲突。
	if b.FTYPOffset == 0 {
		return -100
	}

	score := 0.0

	// 码率相近：同一段视频相邻簇的 NAL 密度不会突变。
	// 只是弱证据 —— 两段码率接近的无关视频也会给正分。
	if a.NALDensity > 0 && b.NALDensity > 0 {
		ratio := math.Min(a.NALDensity, b.NALDensity) / math.Max(a.NALDensity, b.NALDensity)
		switch {
		case ratio >= 0.80:
			score += 2.0
		case ratio >= 0.60:
			score += 1.0
		}
	}
	return score
}
