package feature

import (
	"progrescarve/internal/feature/jpeg"
	"progrescarve/internal/feature/mp4"
)

// Kind 表示一簇数据的内容类型。
//
// Kind 只回答「这是什么」。至于两簇能不能接在一起，不是它的事 ——
// 那是各格式自己的 Feature 和 ScoreNext 管（见 jpeg 包）。
type Kind uint8

const (
	// KindNone 谁都不像：什么都不做的默认值。
	KindNone Kind = iota

	// KindJPEGHeader 某个 JPEG 文件的第一簇，以 FF D8 FF 开头。
	KindJPEGHeader

	// KindJPEGPayload JPEG 的中间/后续碎片：只有统计特征像熵编码流，没有文件头。
	KindJPEGPayload

	// KindMP4Header MP4/MOV 文件的第一簇：开头是 box 结构，第二个盒子名是 ftyp。
	KindMP4Header

	// KindMP4Payload MP4 的媒体数据碎片：mdat 里的 H.264 NAL 流连得成串。
	KindMP4Payload
)

// String 返回日志里用的名字。日志字段名靠它，别随意改。
func (k Kind) String() string {
	switch k {
	case KindJPEGHeader:
		return "jpeg_header"
	case KindJPEGPayload:
		return "jpeg_payload"
	case KindMP4Header:
		return "mp4_header"
	case KindMP4Payload:
		return "mp4_payload"
	default:
		return "none"
	}
}

// Hit 是一簇数据的轻量索引项。
//
// 原始簇数据不保存在 Hit 里：整卡空闲簇上百万，留住数据就是几十 GB。
// 扫描完只留下这些特征，真要核对内容时再按簇号回读磁盘。
type Hit struct {
	Cluster uint32 // 簇号
	Kind    Kind   // 分类结果

	// JPEG 的特征；不是 JPEG 时这里是 nil。
	JPEG *jpeg.Feature

	// MP4 的特征；不是 MP4 时这里是 nil。
	//
	// 各格式各带各的字段，别把它们的字段平铺到这里 —— 一份簇数据只属于一种格式。
	MP4 *mp4.Feature
}

// IsJPEGStart 表示这一簇里出现过 JPEG 起始标记 SOI：它是某个 JPEG 的第一簇，
// 只能当链的起点，不能挂在别的碎片后面。
func (h Hit) IsJPEGStart() bool {
	return h.JPEG != nil && jpeg.IsJPEGStart(*h.JPEG)
}

// IsJPEGEnd 表示这一簇里出现了 JPEG 结束标记 EOI：
// 如果它确实属于某个 JPEG，那它通常就是该文件的最后一簇，后面不能再接。
//
// 方法名一律带格式名：首尾是各格式自己的事（将来就是 IsPNGEnd、IsWebPEnd……），
// 没有「通用首尾」这个概念。判据本身在 jpeg 包里，这里只做空指针保护，不重写一遍。
func (h Hit) IsJPEGEnd() bool {
	return h.JPEG != nil && jpeg.IsJPEGEnd(*h.JPEG)
}

// IsJPEGHead 表示这一簇就是某个 JPEG 的第一簇：SOI 就在簇首（偏移 0）。
//
// 和 IsJPEGStart 的区别要分清：IsJPEGStart 是「簇内出现过 SOI」（可能落在任意偏移），
// IsJPEGHead 是「这一簇从 SOI 开始」。判断目录项给的第一簇对不对，要用后者 ——
// 一个 JPEG 的第一簇，FF D8 必然就在第 0 字节。
func (h Hit) IsJPEGHead() bool {
	return h.JPEG != nil && h.JPEG.SOIOffset == 0
}

// FragmentKind 给出这种格式「后续碎片」对应的分类：
// 文件头分类（jpeg_header / mp4_header）→ 碎片分类（jpeg_payload / mp4_payload）。
//
// 拼接时要的是后者：第一簇是头，它后面接的都该是同格式的碎片。
func (k Kind) FragmentKind() Kind {
	switch k {
	case KindJPEGHeader:
		return KindJPEGPayload
	case KindMP4Header:
		return KindMP4Payload
	default:
		return KindNone
	}
}

// IsJPEG 返回这一簇是不是 JPEG 候选（头或后续碎片都算）。
func (h Hit) IsJPEG() bool {
	return h.Kind == KindJPEGHeader ||
		h.Kind == KindJPEGPayload
}

// IsMP4 返回这一簇是不是 MP4 候选（头或媒体数据碎片都算）。
func (h Hit) IsMP4() bool {
	return h.Kind == KindMP4Header ||
		h.Kind == KindMP4Payload
}

// IsMP4Start 表示这一簇里出现了 ftyp：MP4 的文件头标志。
func (h Hit) IsMP4Start() bool {
	return h.MP4 != nil && mp4.IsMP4Start(*h.MP4)
}

// IsMP4Head 表示这一簇就是 MP4 的第一簇：ftyp 就在簇首（偏移 0）。
func (h Hit) IsMP4Head() bool {
	return h.MP4 != nil && mp4.IsMP4Head(*h.MP4)
}

// HasIndex 表示这一簇里有 moov —— 也就是这个文件的 chunk 偏移表在这簇里。
// 拿到了它，后续簇的位置是算出来的，不是猜出来的。
func (h Hit) HasIndex() bool {
	return h.MP4 != nil && mp4.HasIndex(*h.MP4)
}

// 下面这几个是「不关心具体格式」的问法。carve 一律用这些，别在里面按 Kind 分叉 ——
// 加一种格式只需要在这里多接一行，搜索逻辑一行都不用动。

// IsFileStart 表示这一簇里出现了某种格式的文件头标记。
func (h Hit) IsFileStart() bool { return h.IsJPEGStart() || h.IsMP4Start() }

// IsFileHead 表示这一簇是某个文件的第一簇（文件头标记就在簇首）。
func (h Hit) IsFileHead() bool { return h.IsJPEGHead() || h.IsMP4Head() }

// IsFileEnd 表示这一簇里出现了结束标记，文件数据流到此为止。
//
// 注意 MP4 没有结束标记：它只能靠目录项的 DataLength 判断喂没喂饱。
func (h Hit) IsFileEnd() bool { return h.IsJPEGEnd() }

// IsPayload 表示这一簇是某种格式的「后续碎片」候选 —— 可以接在别人后面。
func (h Hit) IsPayload() bool {
	return h.Kind == KindJPEGPayload || h.Kind == KindMP4Payload
}

// ScoreNext 估计 next 紧跟在 h 后面的可能性，分发到各自格式的打分函数。
// 格式对不上（或有一边没特征）时给 0 —— 那是「没有信息」，不是「冲突」。
func (h Hit) ScoreNext(next Hit) float64 {
	switch {
	case h.JPEG != nil && next.JPEG != nil:
		return jpeg.ScoreNext(*h.JPEG, *next.JPEG)
	case h.MP4 != nil && next.MP4 != nil:
		return mp4.ScoreNext(*h.MP4, *next.MP4)
	default:
		return 0
	}
}

// Classify 对一簇数据做一次分类和特征提取。
//
// 重要：data 只在这个函数调用期间有效。Classify 一返回，调用方就可以释放或复用它 ——
// Hit 里只留特征，不留原始数据。
func Classify(cluster uint32, data []byte) Hit {
	h := Hit{
		Cluster: cluster,
		Kind:    KindNone,
	}

	// 头优先：以 FF D8 FF 开头的簇按文件头算，不再往下判「像不像碎片」。
	if jpeg.IsJPEGHeader(data) {
		f := jpeg.Scan(data)

		h.Kind = KindJPEGHeader
		h.JPEG = &f

		return h
	}

	// MP4 头：ftyp 是硬标记，跟 JPEG 的头平级。
	if mp4.IsMP4Header(data) {
		f := mp4.Scan(data)

		h.Kind = KindMP4Header
		h.MP4 = &f

		return h
	}

	// 都不是头，再看统计特征：JPEG 看 FF 00（数量、密度、以及 FF 后面是不是都补了 00）。
	f := jpeg.Scan(data)

	if jpeg.IsLikelyJPEG(f) {
		h.Kind = KindJPEGPayload
		h.JPEG = &f

		return h
	}

	// ------------------------------------------------------------------
	// MP4 的「媒体数据碎片」识别 —— 暂时停用，代码保留，别删。
	//
	// 为什么停：视频在字节层面没有可判定的局部规律（不像 JPEG 那样有「每个 FF
	// 后面必须补 00」这种在每个位置都成立的判据）。实测只有两种结果：判据一松，
	// 全卡 99% 的簇都判成 MP4 payload；一紧，一个都认不出来（10000 簇里 1 个）。
	// 中间没有稳定区间。
	//
	// 代价还特别大：NAL 扫描要按两种假设把整簇各扫一遍，10 万簇就是几十 GB 的
	// 无效扫描。
	//
	// 现在 MP4 走 carve.go 的 pickLoose（格式引导的兜底），不要求候选自证身份，
	// 所以这块留着只是白烧 CPU。
	//
	// 什么时候能 reopen（满足其一）：
	//   1. 能拿到 moov 的 chunk 偏移表 —— 那时簇序列是算出来的，不用猜内容；
	//   2. 找到别的硬判据，比如「长度必须连起来正好铺满整簇」这类结构约束。
	// 重新启用时，记得把 mp4 的判据单独压测一遍（见 mp4 包里的误判测试）。
	//
	// m := mp4.Scan(data)
	// if mp4.IsLikelyMP4(m) {
	// 	h.Kind = KindMP4Payload
	// 	h.MP4 = &m
	//
	// 	return h
	// }
	// ------------------------------------------------------------------

	return h
}
