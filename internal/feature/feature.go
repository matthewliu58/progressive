package feature

import "progrescarve/internal/feature/jpeg"

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
)

// String 返回日志里用的名字。日志字段名靠它，别随意改。
func (k Kind) String() string {
	switch k {
	case KindJPEGHeader:
		return "jpeg_header"
	case KindJPEGPayload:
		return "jpeg_payload"
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
	//
	// 将来支持 PNG、WebP 时，各加各的字段（PNG、WebP……），
	// 别把所有格式的字段都平铺在这里 —— 一份簇数据只属于一种格式。
	JPEG *jpeg.Feature
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

// IsJPEG 返回这一簇是不是 JPEG 候选（头或后续碎片都算）。
func (h Hit) IsJPEG() bool {
	return h.Kind == KindJPEGHeader ||
		h.Kind == KindJPEGPayload
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

	// 不是头，再看统计特征像不像 JPEG 的后续碎片。
	f := jpeg.Scan(data)

	if jpeg.IsLikelyJPEG(f) {
		h.Kind = KindJPEGPayload
		h.JPEG = &f

		return h
	}

	return h
}
