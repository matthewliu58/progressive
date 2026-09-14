// Package feature 对一块簇数据做内容分类：像哪种格式、是不是文件的第一块。
// 分类词表（Kind）和判定入口（Classify）在这里；各格式的判据在子包（jpeg/、mp4/ 等）。
package feature

import "progrescarve/internal/feature/jpeg"

// Kind 是一块簇数据的内容分类结果。加新格式就是加枚举值。
type Kind uint8

const (
	KindNone        Kind = iota // 谁都不像
	KindJPEGHeader              // JPEG 文件的第一块：FF D8 FF
	KindJPEGPayload             // JPEG 后续碎片：统计特征像熵编码流
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

// detector 把一种格式的判据和它对应的 Kind 绑在一起，供 Classify 循环。
// header 是 magic 级的精确判断（只对文件第一块有意义）；payload 是统计特征级的
// 宽松判断，只说明「像」。
type detector struct {
	header      func(data []byte) bool
	payload     func(data []byte) bool
	kindHeader  Kind
	kindPayload Kind
}

// all 是注册表。新增格式：加 Kind 枚举值、建子包、往这里加一行。
var all = []detector{{
	header:      jpeg.IsJPEGHeader,
	payload:     func(data []byte) bool { return jpeg.IsLikelyJPEG(jpeg.Scan(data)) },
	kindHeader:  KindJPEGHeader,
	kindPayload: KindJPEGPayload,
}}

// Hit 是单个簇的分类结果。带上簇号：碎片重组阶段要按簇重读，结果里没簇号，
// 调用方就得自己再拼一遍。
type Hit struct {
	Cluster uint32 // 簇号
	Kind    Kind   // 分类结果
}

// Classify 循环跑所有注册的格式，返回带簇号的分类结果。
// 头优先：以文件头 magic 开头的簇按头计，不再往下判非头。
func Classify(cluster uint32, data []byte) Hit {
	for _, d := range all {
		if d.header(data) {
			return Hit{Cluster: cluster, Kind: d.kindHeader}
		}
		if d.payload(data) {
			return Hit{Cluster: cluster, Kind: d.kindPayload}
		}
	}
	return Hit{Cluster: cluster}
}
