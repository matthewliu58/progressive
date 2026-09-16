package feature

import "progrescarve/internal/feature/jpeg"

// Kind 表示一个 cluster 的内容类型。
//
// 注意：Kind 只负责“这是什么”。
// 至于两个 cluster 能不能连接，由具体格式的 Feature / ScoreNext 判断。
type Kind uint8

const (
	KindNone Kind = iota

	// JPEG 文件的第一块，通常以 FF D8 FF 开始。
	KindJPEGHeader

	// JPEG 中间/后续碎片。
	KindJPEGPayload
)

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

// Hit 是一个 cluster 的轻量索引信息。
//
// 原始 cluster 数据不保存在 Hit 中。
// 扫描完以后只保留这些 Feature，后续如果需要验证，
// 再根据 Cluster 重新从磁盘读取原始数据。
type Hit struct {
	Cluster uint32
	Kind    Kind

	// JPEG 特征。
	//
	// 如果以后支持 PNG/WebP 等格式，可以继续增加对应的
	// 格式特征；但不要把所有格式的字段都混在这里。
	JPEG *jpeg.Feature
}

// IsJPEGStart 表示这个 cluster 内出现了 JPEG SOI：它是某个 JPEG 的第一块，
// 只能当链的起点，不能挂在别的碎片后面。
func (h Hit) IsJPEGStart() bool {
	return h.JPEG != nil && jpeg.IsJPEGStart(*h.JPEG)
}

// IsJPEGEnd 表示这个 cluster 内出现了 JPEG EOI：
// 如果它确实属于某个 JPEG，那它通常就是该文件的最后一个 cluster，后面不能再接。
//
// 方法名一律带格式名：首尾是各格式自己的事（将来就是 IsPNGEnd、IsWebPEnd……），
// 没有「通用首尾」这个概念。判据本身在 jpeg 包里，这里只做空指针保护，不重写一遍。
func (h Hit) IsJPEGEnd() bool {
	return h.JPEG != nil && jpeg.IsJPEGEnd(*h.JPEG)
}

// IsJPEGHead 表示这个 cluster 就是某个 JPEG 的第一块：SOI 就在簇首（偏移 0）。
//
// 和 IsJPEGStart 的区别要分清：IsJPEGStart 是「簇内出现过 SOI」（可能落在任意偏移），
// IsJPEGHead 是「这一簇从 SOI 开始」。判断目录项给的第一簇对不对，要用后者 ——
// 一个 JPEG 的第一簇，FF D8 必然就在第 0 字节。
func (h Hit) IsJPEGHead() bool {
	return h.JPEG != nil && h.JPEG.SOIOffset == 0
}

// IsJPEG 返回这个 cluster 是否属于 JPEG 候选。
func (h Hit) IsJPEG() bool {
	return h.Kind == KindJPEGHeader ||
		h.Kind == KindJPEGPayload
}

// Classify 对一个 cluster 做一次分类和特征提取。
//
// 重要：data 只在这个函数调用期间存在。
// Classify 返回后，调用方可以立即释放/复用 data。
// Hit 中只保存 Feature，不保存原始 cluster。
func Classify(cluster uint32, data []byte) Hit {
	h := Hit{
		Cluster: cluster,
		Kind:    KindNone,
	}

	// JPEG header 优先。
	if jpeg.IsJPEGHeader(data) {
		f := jpeg.Scan(data)

		h.Kind = KindJPEGHeader
		h.JPEG = &f

		return h
	}

	// 后续 JPEG 碎片。
	f := jpeg.Scan(data)

	if jpeg.IsLikelyJPEG(f) {
		h.Kind = KindJPEGPayload
		h.JPEG = &f

		return h
	}

	return h
}
