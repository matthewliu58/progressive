package recovery

import (
	"log/slog"

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
		if p.HeadHasMOOV {
			p.MOOVCluster, p.MOOVOffset = entry.FirstCluster, f.MOOVOffset
			return p
		}
	}

	// 二、文件尾：按 DataLength 推出最后一簇，从它往回看几簇。
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
		if f := mp4.Scan(data); mp4.HasIndex(f) {
			p.TailHasMOOV = true
			p.MOOVCluster, p.MOOVOffset = cid, f.MOOVOffset
			if p.Codec == "" {
				p.Codec = mp4.CodecTag(data)
			}
			break
		}
	}
	return p
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
