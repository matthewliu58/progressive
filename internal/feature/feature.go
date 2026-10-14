package feature

import (
	"bytes"

	"progrescarve/internal/feature/jpeg"
	"progrescarve/internal/feature/mov"
	"progrescarve/internal/feature/mp4"
	"progrescarve/internal/feature/png"
	"progrescarve/internal/feature/wav"
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

	// KindMP4Header MP4 文件的第一簇：开头是 box 结构，第二个盒子名是 ftyp。
	KindMP4Header

	// KindMP4Payload MP4 的媒体数据碎片：mdat 里的 H.264 NAL 流连得成串。
	KindMP4Payload

	// KindPNGHeader PNG 文件的第一簇，以 8 字节签名开头。
	KindPNGHeader

	// KindPNGPayload PNG 的中间/后续碎片：只有分块结构特征像 PNG，没有文件头。
	KindPNGPayload

	// KindMOVHeader QuickTime 文件的第一簇：开头是 ftyp（现代）或 moov（老式
	// QuickTime，没有 ftyp）。MOV 与 MP4 共用 ISO 基媒体容器，特征数据复用
	// mp4.Feature，只是把 Kind 分开（见 mov 包说明）。
	KindMOVHeader

	// KindMOVPayload MOV 的媒体数据碎片：mdat 里的采样流，跟 MP4 共用容器。
	KindMOVPayload

	// KindWAVHeader WAV（RIFF）文件的第一簇，以 "RIFF" + "WAVE" 开头。
	KindWAVHeader

	// KindWAVPayload WAV 的中间/后续碎片：只有分块结构特征像 WAV，没有文件头。
	KindWAVPayload
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
	case KindPNGHeader:
		return "png_header"
	case KindPNGPayload:
		return "png_payload"
	case KindMOVHeader:
		return "mov_header"
	case KindMOVPayload:
		return "mov_payload"
	case KindWAVHeader:
		return "wav_header"
	case KindWAVPayload:
		return "wav_payload"
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

	// PNG 的特征；不是 PNG 时这里是 nil。
	PNG *png.Feature

	// MOV 的特征；不是 MOV 时这里是 nil。
	//
	// 复用 mp4.Feature：MOV 与 MP4 是同一套 ISO 基媒体容器，盒子结构、moov 地图、
	// 相邻打分全通用（见 mov 包说明），不必另存一份。
	MOV *mp4.Feature

	// WAV 的特征；不是 WAV 时这里是 nil。
	WAV *wav.Feature
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
	case KindPNGHeader:
		return KindPNGPayload
	case KindMOVHeader:
		return KindMOVPayload
	case KindWAVHeader:
		return KindWAVPayload
	default:
		return KindNone
	}
}

// SameFamily 判断两种分类是不是同一个容器家族。
//
// 为什么要它：MP4 和 MOV 是同一套 ISO 基媒体文件格式，盒子结构和 moov 地图完全
// 通用，唯一差别是 ftyp 品牌 —— 而品牌归族并不牢靠，实际文件里家族跟扩展名也常常
// 对不上（把 MP4 品牌的文件存成 .mov 很常见）。所以「第一簇是不是某个文件的头」
// 这类判定不能按 Kind 精确相等来卡：判成 MOV 或 MP4 都说明这一簇是完好的 ISO 容器
// 头，区别只该体现在日志分类上。
//
// 其余格式之间没有家族关系，退回精确相等。
func SameFamily(got, want Kind) bool {
	iso := func(k Kind) bool {
		switch k {
		case KindMP4Header, KindMP4Payload,
			KindMOVHeader, KindMOVPayload:
			return true
		}
		return false
	}
	if iso(got) && iso(want) {
		return true
	}
	return got == want
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

// IsPNG 返回这一簇是不是 PNG 候选（头或后续碎片都算）。
func (h Hit) IsPNG() bool {
	return h.Kind == KindPNGHeader ||
		h.Kind == KindPNGPayload
}

// IsMOV 返回这一簇是不是 MOV 候选（头或媒体数据碎片都算）。
func (h Hit) IsMOV() bool {
	return h.Kind == KindMOVHeader ||
		h.Kind == KindMOVPayload
}

// IsWAV 返回这一簇是不是 WAV 候选（头或后续碎片都算）。
func (h Hit) IsWAV() bool {
	return h.Kind == KindWAVHeader ||
		h.Kind == KindWAVPayload
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

// IsPNGStart 表示这一簇里出现了 PNG 文件头签名：它是某个 PNG 的第一簇，
// 只能当链的起点，不能挂在别的碎片后面。
func (h Hit) IsPNGStart() bool {
	return h.PNG != nil && png.IsPNGStart(*h.PNG)
}

// IsPNGHead 表示这一簇就是某个 PNG 的第一簇：签名就在簇首（偏移 0）。
//
// 和 IsPNGStart 的区别同 JPEG：PNG 签名只可能出现在第 0 字节，所以两者在此等价。
func (h Hit) IsPNGHead() bool {
	return h.PNG != nil && png.IsPNGHead(*h.PNG)
}

// IsPNGEnd 表示这一簇里出现了 IEND：如果它确实属于某个 PNG，那它通常就是该文件的
// 最后一簇，后面不能再接。
func (h Hit) IsPNGEnd() bool {
	return h.PNG != nil && png.IsPNGEnd(*h.PNG)
}

// IsMOVStart 表示这一簇里出现了 MOV 的文件头标记：ftyp（现代）或 moov 在簇首
// （老式 QuickTime，没有 ftyp）。它是某个 MOV 的第一簇，只能当链的起点。
func (h Hit) IsMOVStart() bool {
	return h.MOV != nil && (mp4.IsMP4Start(*h.MOV) || h.MOV.MOOVOffset == 0)
}

// IsMOVHead 表示这一簇就是某个 MOV 的第一簇：文件头标记就在簇首（偏移 0）。
//
// 和 IsMOVStart 的区别同 JPEG：IsMOVStart 是「簇内出现过头标记」（ftyp 可能落在
// 任意偏移），IsMOVHead 是「这一簇从文件头开始」。判断目录项给的第一簇对不对，
// 要用后者 —— 一个 MOV 的第一簇，ftyp 或 moov 必然就在第 0 字节。
//
// 注意这里用 FTYPOffset==0 或 MOOVOffset==0 两种：老式 QuickTime 没有 ftyp、
// 直接从 moov 起，FTYPOffset 是 -1，得看 MOOVOffset。
func (h Hit) IsMOVHead() bool {
	return h.MOV != nil && (h.MOV.FTYPOffset == 0 || h.MOV.MOOVOffset == 0)
}

// IsWAVStart 表示这一簇里出现了 WAV 文件头签名（"RIFF" + "WAVE"）：它是某个 WAV
// 的第一簇，只能当链的起点，不能挂在别的碎片后面。
func (h Hit) IsWAVStart() bool {
	return h.WAV != nil && wav.IsWAVStart(*h.WAV)
}

// IsWAVHead 表示这一簇就是某个 WAV 的第一簇：签名就在簇首（偏移 0）。
//
// 和 IsWAVStart 的区别同 JPEG：WAV 签名只可能出现在第 0 字节，所以两者在此等价。
func (h Hit) IsWAVHead() bool {
	return h.WAV != nil && wav.IsWAVHead(*h.WAV)
}

// IsWAVEnd 表示这一簇是不是 WAV 的最后一簇。
//
// WAV 没有结束标记（不像 JPEG 的 EOI / PNG 的 IEND）："data" 块后面就是文件尾，靠
// 目录项的 DataLength 判断喂没喂饱。所以恒为 false。
func (h Hit) IsWAVEnd() bool {
	return h.WAV != nil && wav.IsWAVEnd(*h.WAV)
}

// 下面这几个是「不关心具体格式」的问法。carve 一律用这些，别在里面按 Kind 分叉 ——
// 加一种格式只需要在这里多接一行，搜索逻辑一行都不用动。

// IsFileStart 表示这一簇里出现了某种格式的文件头标记。
func (h Hit) IsFileStart() bool {
	return h.IsJPEGStart() || h.IsMP4Start() || h.IsPNGStart() || h.IsMOVStart() || h.IsWAVStart()
}

// IsFileHead 表示这一簇是某个文件的第一簇（文件头标记就在簇首）。
func (h Hit) IsFileHead() bool {
	return h.IsJPEGHead() || h.IsMP4Head() || h.IsPNGHead() || h.IsMOVHead() || h.IsWAVHead()
}

// IsFileEnd 表示这一簇里出现了结束标记，文件数据流到此为止。
//
// 注意 MP4 / MOV / WAV 没有结束标记：它们只能靠目录项的 DataLength 判断喂没喂饱。
func (h Hit) IsFileEnd() bool { return h.IsJPEGEnd() || h.IsPNGEnd() }

// IsPayload 表示这一簇是某种格式的「后续碎片」候选 —— 可以接在别人后面。
func (h Hit) IsPayload() bool {
	return h.Kind == KindJPEGPayload ||
		h.Kind == KindMP4Payload ||
		h.Kind == KindPNGPayload ||
		h.Kind == KindMOVPayload ||
		h.Kind == KindWAVPayload
}

// ScoreNext 估计 next 紧跟在 h 后面的可能性，分发到各自格式的打分函数。
// 格式对不上（或有一边没特征）时给 0 —— 那是「没有信息」，不是「冲突」。
func (h Hit) ScoreNext(next Hit) float64 {
	switch {
	case h.JPEG != nil && next.JPEG != nil:
		return jpeg.ScoreNext(*h.JPEG, *next.JPEG)
	case h.MP4 != nil && next.MP4 != nil:
		return mp4.ScoreNext(*h.MP4, *next.MP4)
	case h.PNG != nil && next.PNG != nil:
		return png.ScoreNext(*h.PNG, *next.PNG)
	case h.MOV != nil && next.MOV != nil:
		// MOV 复用 mp4.Feature 与 mp4.ScoreNext：容器是同一套，相邻判据通用。
		return mp4.ScoreNext(*h.MOV, *next.MOV)
	case h.WAV != nil && next.WAV != nil:
		return wav.ScoreNext(*h.WAV, *next.WAV)
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

	// MP4 / MOV 头：都是 ISO 基媒体文件格式，盒子结构同一套。MOV 比 MP4 宽松 ——
	// 老式 QuickTime 没有 ftyp、直接从 moov 开头，mp4.IsMP4Header 认不出，这里用
	// mov.IsMOVHeader 兜底（它收 ftyp 或 moov 两种开头）。两者特征数据共用 mp4.Feature
	// （见 mov 包说明），只把 Kind 分开：没有 ftyp（从 moov 起）或品牌是 qt  一族
	// 归 MOV，其余（isom/mp42/M4V 等品牌）归 MP4。
	if mov.IsMOVHeader(data) {
		setISOHeader(&h, data, mp4.Scan(data))

		return h
	}

	// ftyp 不在簇首、落在簇中间的情况（这一簇前半截是上一个文件的尾巴，MP4 从
	// 中间某个位置开始）：上面只看偏移 0，这种够不着，整簇会被判成 None 漏掉。
	// 这里退一步在整簇里找 ftyp 盒子，找到就从那个位置起按头算。
	//
	// 敢在整簇里搜，是因为 ftyp 是 4 字节、随机撞上的概率约 6e-5 每簇，而且命中后
	// 还要再过一遍 mov.IsMOVHeader 的盒子长度校验，误报再降一档。
	// 反过来 "moov" 不能这么搜：moov 常常出现在文件末尾，整簇一搜会把一堆中间簇
	// 误判成 MOV 头。
	if off, ok := findFTYPBox(data); ok {
		setISOHeader(&h, data[off:], mp4.Scan(data[off:]))

		return h
	}

	// PNG 头：8 字节签名是硬标记，跟上面的头平级。
	if png.IsPNGHeader(data) {
		f := png.Scan(data)

		h.Kind = KindPNGHeader
		h.PNG = &f

		return h
	}

	// WAV 头："RIFF" + 长度 + "WAVE"，跟上面的头平级。靠偏移 8 的 "WAVE" 跟 AVI/WEBP
	// 这类同样用 RIFF 的格式分开。
	if wav.IsWAVHeader(data) {
		f := wav.Scan(data)

		h.Kind = KindWAVHeader
		h.WAV = &f

		return h
	}

	// 都不是头，再看统计特征：JPEG 看 FF 00（数量、密度、以及 FF 后面是不是都补了 00）。
	f := jpeg.Scan(data)

	if jpeg.IsLikelyJPEG(f) {
		h.Kind = KindJPEGPayload
		h.JPEG = &f

		return h
	}

	// PNG 的后续碎片：只能靠「从簇首开始就能顺着分块结构走通」来认。落在 IDAT
	// 压缩流中间的大多数碎片这里会漏掉（见 png.IsLikelyPNG 的注释），它们真正的
	// 身份靠「从文件头顺着走分块」+ verify.go 的真解码确认。
	p := png.Scan(data)

	if png.IsLikelyPNG(p) {
		h.Kind = KindPNGPayload
		h.PNG = &p

		return h
	}

	// WAV 的后续碎片：和 PNG 同理，只能靠「从簇首开始就能顺着 RIFF chunk 走通」来认。
	// 落在 "data" 块中间的采样流（PCM 就是近似随机的字节）孤立看认不出来，真正的
	// 身份靠「从文件头顺着走分块」+ verify.go 的结构校验确认。
	w := wav.Scan(data)

	if wav.IsLikelyWAV(w) {
		h.Kind = KindWAVPayload
		h.WAV = &w

		return h
	}

	// ------------------------------------------------------------------
	// MP4 的「媒体数据碎片」识别 —— 仍然停用，代码保留，别删。
	//
	// 为什么停：视频在字节层面没有可判定的局部规律（不像 JPEG 那样有「每个 FF
	// 后面必须补 00」这种在每个位置都成立的判据）。实测只有两种结果：判据一松，
	// 全卡 99% 的簇都判成 MP4 payload；一紧，一个都认不出来（10000 簇里 1 个）。
	// 中间没有稳定区间。
	//
	// 根子在「孤立地看一簇」这件事上，不在阈值：单位置撞对的概率约 4e-5，而一簇
	// 有 26 万个候选起始位置 —— 期望假阳性十来个。要压住它就得要求连成长串，可
	// 真实视频一簇里往往只有两三个 NAL（4K 的 I 帧就占掉大半簇），真簇反而被杀。
	// 判据的分辨力撑不起这个搜索空间，调阈值只是在「全判成」和「全判不出」之间挪。
	//
	// 代价还特别大：NAL 扫描要按两种假设把整簇各扫一遍，10 万簇就是几十 GB 的
	// 无效扫描。
	//
	// 现在 MP4 不靠这个认：拿得到 moov 的走地图 —— mp4.ParseIndex 算出每个采样在
	// 文件第几字节，carve 的 pickByMoov 按图考试。判据（nalLooksValid）还是这一个，
	// 但考点位置是算出来的，一簇只需定点验几个，假阳性是 4e-5 的 n 次方 ——
	// 跟在 26 万个位置里捞针完全是两回事。拿不到 moov 的才退回 pickLoose
	// （格式引导的兜底，不要求候选自证身份）。
	// 所以这块留着只是白烧 CPU；而且它给的 KindMP4Payload 本来就不可靠，
	// planChain 那边已经不拿它当判据了（探到 moov 才算数）。
	//
	// 什么时候能 reopen：得先找到条件 2 那种硬判据（比如「长度必须连起来正好
	// 铺满整簇」这类结构约束）。条件 1（moov 地图）已经落地，但它是绕开逐簇识别
	// 走的另一条路，不是把这段重新打开 —— 重新打开只会同时付出上面两个代价。
	// 真要启用时，记得把 mp4 的判据单独压测一遍（见 mp4 包里的误判测试）。
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

// setISOHeader 按 ISO 盒子的品牌把 h 定成 MOV 头或 MP4 头。
//
// data 必须从盒子头开始 —— 可能是整簇，也可能是 findFTYPBox 找到位置后切出来的
// 子切片；f 是同一个起点扫出来的特征。两者起点必须一致，否则 box 偏移对不上。
func setISOHeader(h *Hit, data []byte, f mp4.Feature) {
	if !mp4.IsMP4Header(data) || mov.Brand(data) == "mov" {
		h.Kind = KindMOVHeader
		h.MOV = &f
	} else {
		h.Kind = KindMP4Header
		h.MP4 = &f
	}
}

// findFTYPBox 在整簇里找 ftyp 盒子头的偏移，找不到返回 false。
//
// 只认 ftyp（文件类型盒，标志文件起点）。moov 不搜 —— 它常出现在文件末尾，
// 整簇搜会把一堆中间簇误判成文件头（见 Classify 里的说明）。
func findFTYPBox(data []byte) (int, bool) {
	tag := []byte("ftyp")
	idx := 0

	for {
		k := bytes.Index(data[idx:], tag)
		if k < 0 {
			return 0, false
		}

		pos := idx + k
		idx = pos + 1

		// 盒子头是 [长度(4)][类型(4)]：类型前面得有 4 字节长度，才谈得上盒子头。
		if pos < 4 {
			continue
		}
		if mov.IsMOVHeader(data[pos-4:]) {
			return pos - 4, true
		}
	}
}
