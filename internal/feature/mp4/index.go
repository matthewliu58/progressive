package mp4

// 本文件把 moov 解析成一张「文件偏移 → 采样」的地图，并用它考试候选簇。
//
// 为什么要它：mdat 内部没有任何同步结构，孤立地看一簇根本判不出它属于谁
// （见 mp4.go 里那段关于「视频不可判定」的说明）。但 moov 里的 stco/co64 记着
// 每个 chunk 在文件里的偏移，stsc / stsz 记着每个 chunk 装哪几个采样、每个采样
// 多大 —— 三者合起来能算出「第 N 个采样从文件第几字节开始」，是精确值，不是估计。
//
// 于是拼簇从「猜哪个空簇像」变成「按地图核对」：假设候选簇是文件的第 k 簇
// （覆盖文件偏移 [k*簇大小, (k+1)*簇大小)），地图上落在这个范围内的每个采样起点
// 都是一个考点 —— 真实文件在这些位置必然是一个 NAL 的开始。对得上才收。
// 交错存放的两份文件各拿各的地图去考，谁对得上就是谁的。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

const (
	// maxSamples 是采样 / chunk 计数的上限。moov 被写过或本来就是坏的时候，
	// 那个 count 可能是个天文数字，照它建表会直接吃掉几个 GB。
	// 真实素材一轨几十万采样已经很多了（1 小时 30fps 才 10 万出头）。
	maxSamples = 1 << 20

	// maxMoovSize 是能接受的 moov 上限。几十 KB 到几 MB 是常态；
	// 大到离谱说明那个 size 字段本身不可信。
	maxMoovSize = 32 << 20
)

// Sample 是地图上的一个采样（通常就是一帧）。
type Sample struct {
	Offset   uint64 // 在文件里的绝对偏移
	Size     uint32 // 这个采样多少字节
	Keyframe bool   // 是不是关键帧（stss 里点过名的）
	Chunk    uint32 // 属于第几个 chunk（0-based）
}

// Index 是 moov 解析出来的地图。
//
// 只收视频轨：音轨的采样不是 NAL，拿它当考点只会把正确的视频簇否掉。
type Index struct {
	Samples []Sample // 按文件偏移升序
	Codec   string   // 采样项四字符码：avc1 / hvc1 / mp4a……
	NALSize int      // NAL 长度前缀占几个字节（avcC 里记的，通常 4）
	HEVC    bool     // 是不是 HEVC 的 NAL 头格式（类型位位置不同）
}

// Usable 表示这份地图能不能拿来考试（有考点才有意义）。
func (idx Index) Usable() bool { return len(idx.Samples) > 0 }

// CanNALExam 表示这份地图能不能按「采样起点是 NAL 头」来考试。
//
// 只有把采样封装成 NAL 的编码才行（H.264 / HEVC 这一族）：它们的每个采样开头是
// 「长度前缀 + NAL 头」，偏移算出来之后可以逐点验证。而 Apple ProRes、DNxHD
// 这些（MOV 里常见）不是这么封装的，采样开头另有格式 —— 对它们按 NAL 考，
// 正确的簇会一道都过不了，于是全被否掉：比没有地图还糟。
//
// 用白名单而不是黑名单：漏认一个新编码的代价只是「这份不用地图」，而误认一个
// 的代价是把所有正确的簇全否掉。两害相权取前者。
func (idx Index) CanNALExam() bool {
	switch idx.Codec {
	case "avc1", "avc3", "hvc1", "hev1":
		return true
	}
	return false
}

// ParseIndex 从一段完整的 moov（含 box 头）解析出采样地图。
//
// 只认 vide 轨；拿不到采样（没有视频轨、表不全、count 大得离谱）就返回错误 ——
// 解析不出来不是灾难，调用方退回原来的拼法即可，但绝不能拿一张半截的地图去否决簇。
func ParseIndex(moov []byte) (Index, error) {
	if len(moov) < boxHeader {
		return Index{}, errors.New("mp4: moov shorter than a box header")
	}
	typ, bodyStart, bodyEnd, ok := boxBody(moov, 0, len(moov))
	if !ok {
		return Index{}, errors.New("mp4: moov box header unreadable")
	}
	if typ != "moov" {
		return Index{}, fmt.Errorf("mp4: box is %q, want \"moov\"", typ)
	}

	var (
		idx    = Index{NALSize: 4}
		found  bool
		bestN  int
		bestOf []Sample
	)
	forEachBox(moov, bodyStart, bodyEnd, func(bt string, bs, be int) bool {
		if bt != "trak" {
			return true
		}
		ti, ok := parseTrak(moov, bs, be)
		if !ok {
			return true
		}
		// 多个视频轨时取采样最多的那条（通常只有一条）。
		if !found || len(ti.samples) > bestN {
			bestOf, bestN = ti.samples, len(ti.samples)
			idx.Codec, idx.NALSize, idx.HEVC = ti.codec, ti.nalSize, ti.hevc
			found = true
		}
		return true
	})
	if !found {
		return Index{}, errors.New("mp4: no usable video track in moov")
	}

	idx.Samples = bestOf
	// chunk 偏移理论上升序，但写坏的表里不一定；排序后二分才站得住。
	sort.Slice(idx.Samples, func(i, j int) bool { return idx.Samples[i].Offset < idx.Samples[j].Offset })
	return idx, nil
}

// boxBody 读 off 处那个 box 的头，返回类型和 body 范围。
func boxBody(d []byte, off, end int) (typ string, bodyStart, bodyEnd int, ok bool) {
	if off < 0 || off+boxHeader > end {
		return "", 0, 0, false
	}
	typ = string(d[off+4 : off+8])
	size, hdr, ok := boxSize(d, off, end)
	if !ok {
		return typ, 0, 0, false
	}
	return typ, off + hdr, off + size, true
}

// boxSize 读出 box 的总长度和头长，处理 64 位长度（size == 1）和「到末尾」（size == 0）。
func boxSize(d []byte, off, end int) (size, hdr int, ok bool) {
	size = int(binary.BigEndian.Uint32(d[off:]))
	hdr = boxHeader
	switch size {
	case 1:
		if off+16 > end {
			return 0, 0, false
		}
		size = int(binary.BigEndian.Uint64(d[off+8:]))
		hdr = 16
	case 0:
		size = end - off
	}
	if size < hdr || off+size > end {
		return 0, 0, false
	}
	return size, hdr, true
}

// forEachBox 遍历 [start,end) 里的一串 box；fn 返回 false 就停。
//
// 长度说不通就整个停下：那说明已经不是在 box 的地盘里了（多半撞进了 mdat
// 的媒体数据），继续顺下去只会记下一堆假 box。
func forEachBox(d []byte, start, end int, fn func(typ string, bodyStart, bodyEnd int) bool) {
	off := start
	for off+boxHeader <= end {
		size, hdr, ok := boxSize(d, off, end)
		if !ok {
			return
		}
		if !fn(string(d[off+4:off+8]), off+hdr, off+size) {
			return
		}
		off += size
	}
}

// trackInfo 是一条 trak 解析出来的东西。
type trackInfo struct {
	isVideo bool
	codec   string
	nalSize int
	hevc    bool
	samples []Sample
}

// parseTrak 解析一条 trak：要 hdlr 说它是视频轨，且 stbl 里的表齐到能算出采样偏移。
func parseTrak(d []byte, start, end int) (trackInfo, bool) {
	ti := trackInfo{nalSize: 4}
	stbl := false
	forEachBox(d, start, end, func(bt string, bs, be int) bool {
		if bt != "mdia" {
			return true
		}
		forEachBox(d, bs, be, func(mt string, ms, me int) bool {
			switch mt {
			case "hdlr":
				// full box 头 4 字节 + pre_defined(4) + handler_type(4)
				if me >= ms+12 {
					ti.isVideo = string(d[ms+8:ms+12]) == "vide"
				}
			case "minf":
				forEachBox(d, ms, me, func(nt string, ns, ne int) bool {
					if nt != "stbl" {
						return true
					}
					parseSTBL(d, ns, ne, &ti)
					stbl = true
					return true
				})
			}
			return true
		})
		return true
	})
	if !ti.isVideo || !stbl || len(ti.samples) == 0 {
		return ti, false
	}
	return ti, true
}

// parseSTBL 读采样表箱子里的四张表，算出每个采样在文件里的偏移。
//
// 四张表的分工：stco/co64 给 chunk 起点，stsc 说每个 chunk 装几个采样，
// stsz 给每个采样多大，stss 点出关键帧。前三张缺一就算不出偏移。
func parseSTBL(d []byte, start, end int, ti *trackInfo) {
	var (
		sizes     []uint32
		stsc      []stscEntry
		chunkOffs []uint64
		keyframes map[uint32]bool
		codec     string
		nalSize   int
		hevc      bool
	)
	forEachBox(d, start, end, func(bt string, bs, be int) bool {
		switch bt {
		case "stsd":
			codec, nalSize, hevc = parseSTSD(d, bs, be)
		case "stsz":
			sizes = parseSTSZ(d, bs, be)
		case "stsc":
			stsc = parseSTSC(d, bs, be)
		case "stco":
			chunkOffs = parseChunkOffsets(d, bs, be, false)
		case "co64":
			chunkOffs = parseChunkOffsets(d, bs, be, true)
		case "stss":
			keyframes = parseSTSS(d, bs, be)
		}
		return true
	})

	if len(sizes) == 0 || len(stsc) == 0 || len(chunkOffs) == 0 {
		return
	}

	// 一个 chunk 内的采样是首尾相接的：起点已知，累加大小就得到下一个的偏移。
	samples := make([]Sample, 0, len(sizes))
	no := uint32(0) // 采样全局序号，0-based（stss 里是 1-based，查表时 +1）
	for ci := 0; ci < len(chunkOffs) && int(no) < len(sizes); ci++ {
		off := chunkOffs[ci]
		for k := uint32(samplesPerChunk(stsc, uint32(ci+1))); k > 0 && int(no) < len(sizes); k-- {
			sz := sizes[no]
			samples = append(samples, Sample{
				Offset:   off,
				Size:     sz,
				Keyframe: keyframes[no+1],
				Chunk:    uint32(ci),
			})
			off += uint64(sz)
			no++
		}
	}

	ti.samples = samples
	ti.codec = codec
	ti.hevc = hevc
	if nalSize > 0 {
		ti.nalSize = nalSize
	}
}

// stscEntry 是 stsc 里的一条：从第 firstChunk 个 chunk 起，每个装 samplesPerChunk 个采样。
type stscEntry struct {
	firstChunk      uint32 // 1-based
	samplesPerChunk uint32
}

// samplesPerChunk 查第 chunk（1-based）个 chunk 装几个采样：取最后一条覆盖它的记录。
func samplesPerChunk(entries []stscEntry, chunk uint32) uint32 {
	spc := uint32(0)
	for _, e := range entries {
		if e.firstChunk > chunk {
			break
		}
		spc = e.samplesPerChunk
	}
	return spc
}

// parseSTSD 读采样项：拿四字符码（avc1 / hvc1 …）和 NAL 长度前缀占几字节。
//
// 长度前缀不是定死的 4 —— 它记在 avcC 里（lengthSizeMinusOne 的低两位 + 1）。
// 拿不到就按 4 走：绝大多数文件都是 4，猜错只是这一簇考不过，不会把对的否掉。
func parseSTSD(d []byte, start, end int) (codec string, nalSize int, hevc bool) {
	nalSize = 4
	if end-start < 8 {
		return "", nalSize, false
	}
	count := binary.BigEndian.Uint32(d[start+4 : start+8])
	pos := start + 8
	for i := uint32(0); i < count && pos+16 <= end; i++ {
		esz := int(binary.BigEndian.Uint32(d[pos:]))
		if esz < 16 || pos+esz > end {
			break
		}
		if codec == "" {
			codec = string(d[pos+4 : pos+8])
			switch codec {
			case "hvc1", "hev1":
				hevc = true
			}
		}
		// avcC 就在这一项里面：configurationVersion 等 4 个字节之后才是
		// lengthSizeMinusOne，所以是跳过 "avcC" 四个字母再走 4 字节。
		if j := bytes.Index(d[pos:pos+esz], []byte("avcC")); j >= 0 {
			if p := pos + j + 8; p < pos+esz {
				nalSize = int(d[p]&0x03) + 1
			}
		}
		pos += esz
	}
	return codec, nalSize, hevc
}

// parseSTSZ 读每个采样多大。sample_size 非 0 表示所有采样一样大，省掉一张表。
func parseSTSZ(d []byte, start, end int) []uint32 {
	if end-start < 12 {
		return nil
	}
	uniform := binary.BigEndian.Uint32(d[start+4 : start+8])
	count := binary.BigEndian.Uint32(d[start+8 : start+12])
	if count == 0 || count > maxSamples {
		return nil
	}
	out := make([]uint32, 0, count)
	if uniform != 0 {
		for i := uint32(0); i < count; i++ {
			out = append(out, uniform)
		}
		return out
	}
	pos := start + 12
	for i := uint32(0); i < count && pos+4 <= end; i++ {
		out = append(out, binary.BigEndian.Uint32(d[pos:]))
		pos += 4
	}
	return out
}

// parseSTSC 读 stsc：每 12 字节一条（first_chunk, samples_per_chunk, 描述索引）。
func parseSTSC(d []byte, start, end int) []stscEntry {
	if end-start < 8 {
		return nil
	}
	count := binary.BigEndian.Uint32(d[start+4 : start+8])
	if count == 0 || count > maxSamples {
		return nil
	}
	out := make([]stscEntry, 0, count)
	pos := start + 8
	for i := uint32(0); i < count && pos+12 <= end; i++ {
		out = append(out, stscEntry{
			firstChunk:      binary.BigEndian.Uint32(d[pos:]),
			samplesPerChunk: binary.BigEndian.Uint32(d[pos+4:]),
		})
		pos += 12
	}
	return out
}

// parseChunkOffsets 读每个 chunk 从文件第几字节开始：stco 是 32 位，co64 是 64 位。
func parseChunkOffsets(d []byte, start, end int, co64 bool) []uint64 {
	if end-start < 8 {
		return nil
	}
	count := binary.BigEndian.Uint32(d[start+4 : start+8])
	if count == 0 || count > maxSamples {
		return nil
	}
	step := 4
	if co64 {
		step = 8
	}
	out := make([]uint64, 0, count)
	pos := start + 8
	for i := uint32(0); i < count && pos+step <= end; i++ {
		if co64 {
			out = append(out, binary.BigEndian.Uint64(d[pos:]))
		} else {
			out = append(out, uint64(binary.BigEndian.Uint32(d[pos:])))
		}
		pos += step
	}
	return out
}

// parseSTSS 读关键帧的采样序号（1-based）。没有这张表也不影响算偏移。
func parseSTSS(d []byte, start, end int) map[uint32]bool {
	if end-start < 8 {
		return nil
	}
	count := binary.BigEndian.Uint32(d[start+4 : start+8])
	if count == 0 || count > maxSamples {
		return nil
	}
	out := make(map[uint32]bool, count)
	pos := start + 8
	for i := uint32(0); i < count && pos+4 <= end; i++ {
		out[binary.BigEndian.Uint32(d[pos:])] = true
		pos += 4
	}
	return out
}

// Match 拿一簇数据去考试：假设它覆盖文件偏移 [fileOff, fileOff+len(data))。
//
// 地图上落在这个范围内的每个采样起点都是一个考点 —— 真实文件在这些位置必然是
// 一个 NAL 的开始（长度前缀 + NAL 头，见 nalLooksValid）。返回考点数和通过数：
// 有考点却没全过，就说明这一簇不是这个文件的这一段。
//
// checked == 0 表示「这一段地图上没有考点」（比如整簇都落在一个大采样内部），
// 那是没有信息，不是否定的证据 —— 该怎么判由调用方决定，这里不替它拿主意。
func (idx Index) Match(data []byte, fileOff uint64) (checked, passed int) {
	if !idx.Usable() || len(data) == 0 {
		return 0, 0
	}
	// 长度前缀只支持 2 和 4（见 nalLength），其余一律按 4 处理。
	hyp := nalHypothesis{lengthSize: 4, hevc: idx.HEVC}
	if idx.NALSize == 2 {
		hyp.lengthSize = 2
	}

	endOff := fileOff + uint64(len(data))
	i := sort.Search(len(idx.Samples), func(i int) bool { return idx.Samples[i].Offset >= fileOff })
	for ; i < len(idx.Samples) && idx.Samples[i].Offset < endOff; i++ {
		off := int(idx.Samples[i].Offset - fileOff)
		if off < 0 || off+hyp.lengthSize+1 > len(data) {
			continue // 这个采样起点落在簇尾之外，读不全，不作数
		}
		checked++
		if nalLooksValid(data, off, hyp) {
			passed++
		}
	}
	return checked, passed
}

// ChecksIn 数一数 [fileOff, fileOff+size) 这一段地图上有几个考点。
// 日志用：事后要能看出「这一簇是考过的」还是「地图上压根没有考点、盲收的」。
func (idx Index) ChecksIn(fileOff, size uint64) int {
	if !idx.Usable() {
		return 0
	}
	endOff := fileOff + size
	n := 0
	i := sort.Search(len(idx.Samples), func(i int) bool { return idx.Samples[i].Offset >= fileOff })
	for ; i < len(idx.Samples) && idx.Samples[i].Offset < endOff; i++ {
		n++
	}
	return n
}

// MoovSize 读出一段数据里 moov box 的总长度，用来判断它有没有跨簇、要读几簇。
// 传 moov 在这一段里的起始偏移。
func MoovSize(d []byte, off int) (uint64, error) {
	if off < 0 || off+boxHeader > len(d) {
		return 0, errors.New("mp4: moov header out of range")
	}
	if string(d[off+4:off+8]) != "moov" {
		return 0, fmt.Errorf("mp4: box at %d is %q, want \"moov\"", off, d[off+4:off+8])
	}
	size, _, ok := boxSize(d, off, len(d))
	if !ok {
		return 0, errors.New("mp4: moov size unreadable")
	}
	if uint64(size) > maxMoovSize {
		return 0, fmt.Errorf("mp4: moov size %d exceeds %d", size, maxMoovSize)
	}
	return uint64(size), nil
}
