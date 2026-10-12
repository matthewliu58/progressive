// Package mov 认 QuickTime（.mov），并回答「这份文件的 moov 地图能不能拿来考试」。
//
// 先把跟 mp4 包的关系说清楚，免得重复做一遍：
//
// MOV 和 MP4 是同一套 ISO 基媒体文件格式（ISO/IEC 14496-12）—— 盒子结构是同一份
// 规范，moov 里那几张采样表连名字都一样：stco/co64（chunk 偏移）、stsc（每个
// chunk 装几个采样）、stsz（每个采样多大）、stss（关键帧）。所以「拿 moov 算出
// 每个采样在文件第几字节」这条 MOV 完全通用，直接用 mp4.ParseIndex，一行都不用
// 另写；连「头是不是 ftyp」这个判断，两者也是同一条。
//
// 真正的差别在编码：MP4 的视频轨几乎总是 H.264 / HEVC，采样是 NAL 封装的
// （长度前缀 + NAL 头）；而 MOV 除了这两种，还常见 Apple ProRes、DNxHD/DNxHR、
// Animation、Photo-JPEG 等 —— 这些不是 NAL 封装的，采样开头另有格式。对它们
// 按 NAL 考试，正确的簇一道都过不了，整片被否掉。
//
// 所以本包不重复实现地图，只补那一句判断：这份 MOV 的编码能不能按 NAL 考。
// 不能考就别考 —— 退回格式引导的兜底，也好过拿不适用的判据否掉正确的簇。
package mov

import (
	"bytes"
	"encoding/binary"
)

const (
	// boxHeader 是一个盒子的头长：4 字节长度 + 4 字节类型。
	boxHeader = 8

	// ftypMaxSize 是 ftyp 盒子的合理上限。它通常只有 20~40 字节（品牌列表），
	// 大到离谱说明这里不是文件头。
	ftypMaxSize = 4096

	// codecScanMax 是找编码四字符码时最多看多少字节。stsd 在 moov 里、离文件头
	// 不远，没必要整簇搜一遍。
	codecScanMax = 64 << 10
)

// nalCodecs 是把采样封装成 NAL 的编码：采样开头是「长度前缀 + NAL 头」，
// 偏移算出来之后可以逐点验证。
var nalCodecs = []string{"avc1", "avc3", "hvc1", "hev1"}

// movCodecs 是 MOV 里常见的采样项四字符码，含那些不是 NAL 封装的。
//
// 列它只为探测和日志：认出 ProRes / DNxHD，才能在日志里说清「这份文件为什么
// 用不了地图」，而不是莫名其妙地跳过。能不能考走的是 nalCodecs 白名单，
// 不靠这张表 —— 漏列一个新编码不影响正确性，最多是日志认不出来。
var movCodecs = []string{
	// H.264 / HEVC：跟 MP4 一样，能按 NAL 考。
	"avc1", "avc3", "hvc1", "hev1",
	// Apple ProRes 一族：帧内是自己一套头，不是 NAL。
	"apcn", "apcs", "apco", "apch", "ap4h",
	// Avid DNxHD / DNxHR。
	"AVdn", "AVdh",
	// QuickTime Animation / Photo-JPEG / Sorenson。
	"rle ", "jpeg", "svq1", "svq3",
	// 音频：这一轨本来就不参与考试（mp4.ParseIndex 只取 vide 轨）。
	"mp4a",
}

// IsMOVHeader 判断 data 是不是一个 MOV / QTFF 文件的开头。
//
// 这是本包跟 mp4 真正有差别的地方之一：mp4.IsMP4Header 要求第一个盒子必须是
// ftyp（MP4 规范强制），但 MOV 不能照搬 —— ftyp 是 QTFF 后来为对齐 ISO 才加的，
// 传统 QuickTime 布局没有它，文件直接从 moov 开头。拿「必须是 ftyp」去卡，
// 这类好文件会被判成不是文件头。
//
// 所以这里两种开头都收：ftyp（现在常见）和 moov（老 QuickTime）。
// 两者负判据不同：ftyp 只有几十字节，大到离谱说明不是头；moov 本身可以很大，
// 所以只要求它的长度在这一簇之内说得通。
func IsMOVHeader(data []byte) bool {
	if len(data) < boxHeader {
		return false
	}
	size := uint64(binary.BigEndian.Uint32(data))
	if size < boxHeader || size > uint64(len(data)) {
		return false
	}
	switch string(data[4:8]) {
	case "ftyp":
		return size <= ftypMaxSize
	case "moov":
		return true
	}
	return false
}

// brands 是 ftyp 里常见的品牌字符串。MOV 通常是 'qt  '（后面两个空格），
// MP4 是 isom / mp42 / M4V  这一族。
//
// 只用来在日志里说清这份文件是哪一族的：能不能考地图取决于编码，不取决于品牌
// （见 CanNALExam 和 nalCodecs）。
var brands = []struct {
	family string
	tag    string
}{
	{"mov", "qt  "},
	{"mp4", "isom"},
	{"mp4", "mp42"},
	{"mp4", "avc1"},
	{"mp4", "M4V "},
}

// Brand 读出这份文件属于哪一族（"mov" / "mp4"），认不出返回空串。
//
// 读的是 ftyp 盒子里偏移量固定的 major brand —— 它就落在盒子头 8 字节之后，
// 是规范指定的权威字段：文件自己声明「我是哪一族」。没有 ftyp 就没有品牌可言
// （传统 QuickTime 直接从 moov 起，那种归 MOV 不靠品牌，见 Classify 里
// 的 !mp4.IsMP4Header 分支）。
//
// 早先这里是在前 64KB 里子串搜索这张表，被两个问题打回来了：
//   - 位置无关：只要那 4 个字节出现在任何地方（udta 元数据、甚至是 mdat 里的
//     媒体数据）就算命中，于是兼容品牌里带 "qt  " 的 .mp4 会被判成 MOV；
//   - 顺序依赖：命中即返回，文件里同时有 isom 和 qt  时，族由这张表的排列顺序
//     决定，而不是文件自己声明的主品牌。
//
// 它本来只是「看着像」的猜测，却被 Classify 拿去决定 Kind，再被 recovery 的
// checkFirstCluster 跟扩展名做精确相等比较 —— 一个粗糙的猜测最终成了整份文件被
// 丢弃的理由。改成解析 major brand 之后，位置确定、语义确定，不再靠猜。
func Brand(data []byte) string {
	// 盒子头 8 字节 + major brand 4 字节。
	if len(data) < boxHeader+4 {
		return ""
	}
	if uint64(binary.BigEndian.Uint32(data)) < boxHeader {
		return ""
	}
	// 没有 ftyp 就没有品牌可言：传统 QuickTime 直接从 moov 起，
	// 那种归 MOV 走的不是品牌这条路。
	if string(data[4:8]) != "ftyp" {
		return ""
	}
	major := string(data[boxHeader : boxHeader+4])
	for _, b := range brands {
		if b.tag == major {
			return b.family
		}
	}
	return ""
}

// CodecTag 在数据里找采样项的四字符码，找不到返回空串。
//
// 为什么要单独探它：mp4.CodecTag 只认 H.264 / HEVC / AAC 那几个，MOV 里的
// Apple ProRes（apcn…）、DNxHD（AVdn）它认不出来 —— 而「这是 ProRes」才是
// 「这份文件为什么用不了地图」的答案，说得出这句，日志才解释得通。
func CodecTag(data []byte) string {
	limit := len(data)
	if limit > codecScanMax {
		limit = codecScanMax
	}
	for _, tag := range movCodecs {
		if bytes.Index(data[:limit], []byte(tag)) >= 0 {
			return tag
		}
	}
	return ""
}

// CanNALExam 判断这个采样项编码是不是 NAL 封装的。
//
// 白名单，理由同 mp4.Index.CanNALExam：漏认一个新编码的代价只是「这份不用地图」，
// 而误认一个的代价是把所有正确的簇全否掉。两害相权取前者。
func CanNALExam(codec string) bool {
	for _, c := range nalCodecs {
		if codec == c {
			return true
		}
	}
	return false
}
