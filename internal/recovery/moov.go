package recovery

import (
	"bytes"
	"log/slog"

	"progrescarve/internal/feature/mov"
	"progrescarve/internal/feature/mp4"

	fsinit "progrescarve/internal/fs/finit"
)

// 这个文件的活只有一件：探一探某个 MP4 的索引（moov）还在不在、在哪一簇。
//
// 为什么先做这一步：moov 里的 stco/co64 记着每个 chunk 在文件里的偏移，拿到它，
// 后续簇的位置是「算」出来的，不用像 carve 那样靠距离和打分去猜。但这条路值不值得
// 投入，取决于 moov 到底还能不能捞着 —— 所以先只探不打，跑一轮看命中率。
//
// 探测的两处位置对应两种常见布局：
//   - 文件头（相机、做了 faststart 的）：ftyp 之后紧跟着 moov，再是 mdat；
//   - 文件尾（手机录的）：先写 mdat，录完才回头补 moov，所以它在最后几簇。

// moovTailProbe 是往回探多少簇。moov 大的时候会跨好几簇（几百 KB 到几 MB 都有），
// 但它总是贴着文件尾，所以从最后一簇往回看几簇就够。
const moovTailProbe = 4

// boxHeaderSize 是盒子头的长度字段占几字节：盒子是 [长度(4)][类型(4)]，
// 所以搜到 "moov" 这四个字节后，盒子头从它前面 boxHeaderSize 字节开始。
const boxHeaderSize = 4

// moovProbe 是一次探测的结论。
type moovProbe struct {
	FirstCluster uint32 // 目录项给的第一簇
	TailCluster  uint32 // 按 DataLength 推出来的最后一簇

	HeadHasFTYP bool // 第一簇开头是 ftyp：文件头还在
	HeadHasMOOV bool // 第一簇里就有 moov（文件头布局）
	HeadHasMDAT bool // 第一簇里就有 mdat
	TailHasMOOV bool // 末尾几簇里有 moov（文件尾布局）

	MOOVCluster uint32 // moov 所在的簇；0 = 没找到
	MOOVOffset  int    // moov 在该簇内的偏移；-1 = 没找到

	// Codec 是采样项的四字符码（avc1 = H.264，hvc1/hev1 = HEVC，mp4a = AAC）。
	// 探它是因为 H.264 和 HEVC 的 NAL 头不一样，识别器得先知道面对的是哪种。
	Codec string

	ClustersProbed int // 这次探了几个簇（记着看 I/O 代价）
}

// Found 表示这一轮探到了 moov。
func (p moovProbe) Found() bool { return p.MOOVCluster != 0 }

// probeMoov 探一个 MP4 条目的 moov 在哪。
//
// 头里找到了就不看尾巴（省读）；读不出来（簇越界、介质错误）一律跳过，
// 探不到不是错误 —— 它只影响「后面能不能算」，不影响现在的恢复流程。
func probeMoov(parser fsinit.FileSystemParser, entry fsinit.FileEntryItem) moovProbe {
	p := moovProbe{FirstCluster: entry.FirstCluster, MOOVOffset: -1}

	clusterSize, firstCid, totalCid := parser.ClusterHeapRange()
	if clusterSize == 0 || totalCid == 0 {
		return p
	}
	lastCid := firstCid + totalCid

	// 一、文件头：ftyp 在不在、moov 是不是紧跟其后。
	if data, err := parser.ReadCluster(entry.FirstCluster); err == nil {
		p.ClustersProbed++
		f := mp4.Scan(data)
		p.HeadHasFTYP = mp4.IsMP4Header(data)
		p.HeadHasMOOV = mp4.HasIndex(f)
		p.HeadHasMDAT = f.MDATOffset >= 0
		p.Codec = mp4.CodecTag(data)
		if p.Codec == "" {
			// MOV 里常见的 Apple 编码（ProRes / DNxHD…）mp4 那边认不出来，
			// 由 mov 包补认：说出「这是 ProRes」才解释得通为什么这份文件用不了地图。
			p.Codec = mov.CodecTag(data)
		}
		if p.HeadHasMOOV {
			p.MOOVCluster, p.MOOVOffset = entry.FirstCluster, f.MOOVOffset
			return p
		}
	}

	// 二、文件尾：按 DataLength 推出最后一簇，从它往回看几簇。
	//
	// 手机视频的 moov 起点通常不簇对齐（ftyp+mdat 长度随机），簇首还在 mdat 的数据
	// 里，只从偏移 0 按 box 解析会漏掉 —— 曾经因此对手机视频静默退化到启发式。
	// 现在每簇先从偏移 0 解析一遍，失败再在簇内全文搜 "moov" 标记、从盒子头重解析。
	fileClusters := (entry.DataLength + clusterSize - 1) / clusterSize
	if fileClusters == 0 {
		return p
	}
	tail := entry.FirstCluster + uint32(fileClusters) - 1
	if tail >= lastCid {
		tail = lastCid - 1 // DataLength 被写坏时别越界
	}
	p.TailCluster = tail

	for i := 0; i < moovTailProbe; i++ {
		if uint32(i) > tail {
			break
		}
		cid := tail - uint32(i)
		if cid < firstCid || cid <= entry.FirstCluster {
			break
		}
		data, err := parser.ReadCluster(cid)
		if err != nil {
			continue
		}
		p.ClustersProbed++

		// 先按老办法：从簇首按 box 结构解析。moov 正好簇对齐时（相机视频）走这条。
		if f := mp4.Scan(data); mp4.HasIndex(f) {
			p.setTail(cid, f.MOOVOffset, data)
			break
		}

		// 手机视频的 moov 起点通常不簇对齐（ftyp+mdat 长度随机），这一簇开头还在
		// mdat 的数据里，从偏移 0 解析必然失败。
		//
		// 退一步在簇内搜 "moov" 标记，再从盒子头重新解析一遍：只有真能解析出索引
		// 才算数 —— "moov" 四个字节随机撞上的概率约 6e-5 每簇，而且撞上也过不了
		// 下面 box 结构这一关，不会引入误报。
		if k := bytes.Index(data, []byte("moov")); k >= boxHeaderSize {
			start := k - boxHeaderSize
			if f := mp4.Scan(data[start:]); mp4.HasIndex(f) {
				p.setTail(cid, int(start), data[start:])
				break
			}
		}
	}
	return p
}

// setTail 记下 moov 落在哪一簇、簇内第几字节，并补认编码（mp4 认不出的 Apple
// 编码交给 mov 包补，见 probeMoov 头部的说明）。
//
// data 必须和 offset 同源：从 moov 盒子头开始，后面 CodecTag 才探得对。
func (p *moovProbe) setTail(cid uint32, offset int, data []byte) {
	p.TailHasMOOV = true
	p.MOOVCluster, p.MOOVOffset = cid, offset
	if p.Codec == "" {
		p.Codec = mp4.CodecTag(data)
	}
	if p.Codec == "" {
		p.Codec = mov.CodecTag(data)
	}
}

// logMoovProbe 记一条探测结果。
//
// 级别用 Info：这是有目的的探测，每个 MP4 条目一条，跑完就靠它统计命中率 ——
// 躲进 Debug 里等于没做。
func logMoovProbe(logger *slog.Logger, pre, path string, p moovProbe) {
	if logger == nil {
		return
	}
	logger.Info("mp4 moov probe",
		slog.String("pre", pre),
		slog.String("entry", path),
		slog.Uint64("first_cluster", uint64(p.FirstCluster)),
		slog.Uint64("tail_cluster", uint64(p.TailCluster)),
		slog.Bool("head_ftyp", p.HeadHasFTYP),
		slog.Bool("head_moov", p.HeadHasMOOV),
		slog.Bool("head_mdat", p.HeadHasMDAT),
		slog.Bool("tail_moov", p.TailHasMOOV),
		slog.Uint64("moov_cluster", uint64(p.MOOVCluster)),
		slog.Int("moov_offset", p.MOOVOffset),
		slog.String("codec", p.Codec),
		slog.Int("probed", p.ClustersProbed))
}

// moovMaxClusters 是读 moov 时最多顺着往下读几簇。
//
// moov 大的时候会跨好几簇（几百 KB 到几十 MB 都有），但它内部是连续的，顺着读几簇
// 就够；读这么多还凑不齐，说明那个 size 字段本身不可信。
// 上限按 index.go 的 maxMoovSize=32MB 动态计算：32MB / clusterSize。
// 这样 32K 簇是 1024 簇，128K 簇是 256 簇，统一卡在 32MB 不会多读。
func moovMaxClusters(clusterSize uint64) uint64 {
	const maxBytes = 32 << 20 // 32MB，与 mp4.maxMoovSize 对齐
	if clusterSize == 0 {
		return 0
	}
	return maxBytes / clusterSize
}

// loadMoovIndex 把探测指出的那个 moov 整段读出来，解析成「偏移 → 采样」地图
// （见 mp4.ParseIndex）。拿到了它，后续簇是「考」出来的，不是猜出来的。
//
// 读不全、解析出错一律返回 nil：没有地图只是退回原来的拼法，而一张半截的地图
// 会去否决正确的簇 —— 后者比没有地图糟得多。
func loadMoovIndex(parser fsinit.FileSystemParser, clusterSize uint64, p moovProbe) *mp4.Index {
	if !p.Found() || clusterSize == 0 {
		return nil
	}
	first, err := parser.ReadCluster(p.MOOVCluster)
	if err != nil {
		return nil
	}
	size, err := mp4.MoovSize(first, p.MOOVOffset)
	if err != nil {
		return nil
	}
	need := uint64(p.MOOVOffset) + size
	if need <= uint64(len(first)) {
		// moov 整个就在这一簇里，不用再往下读。
		idx, err := mp4.ParseIndex(first[p.MOOVOffset:need])
		if err != nil {
			return nil
		}
		return &idx
	}

	buf := make([]byte, 0, need)
	buf = append(buf, first...)
	maxClusters := moovMaxClusters(clusterSize)
	for cid := p.MOOVCluster + 1; uint64(len(buf)) < need; cid++ {
		if uint64(len(buf))/clusterSize >= maxClusters {
			return nil
		}
		next, err := parser.ReadCluster(cid)
		if err != nil {
			return nil
		}
		buf = append(buf, next...)
	}
	idx, err := mp4.ParseIndex(buf[p.MOOVOffset:need])
	if err != nil {
		return nil
	}
	return &idx
}

// logMoovMap 记一条「这份文件有没有拿到 moov 地图、地图上多少个考点」。
//
// 级别 Info：这是「后面靠算还是靠猜」的分岔点，用上地图的文件各一条 ——
// 跑完能直接统计出多少文件从猜升级成了算。
func logMoovMap(logger *slog.Logger, path string, p moovProbe, idx *mp4.Index) {
	if logger == nil {
		return
	}
	attrs := []any{
		slog.String("entry", path),
		slog.Uint64("moov_cluster", uint64(p.MOOVCluster)),
		slog.Int("moov_offset", p.MOOVOffset),
		slog.String("codec", p.Codec),
		slog.Bool("usable", idx != nil && idx.Usable()),
	}
	if idx != nil {
		attrs = append(attrs,
			slog.Int("samples", len(idx.Samples)),
			slog.Int("nal_size", idx.NALSize),
			slog.Bool("hevc", idx.HEVC))
	}
	logger.Info("mp4 moov map", attrs...)
}
