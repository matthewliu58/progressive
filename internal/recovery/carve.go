package recovery

import (
	"log/slog"
	"slices"
	"strings"

	"progrescarve/internal/feature"

	fsinit "progrescarve/internal/fs/finit"
)

// 本文件只干一件事：链断了、后半段没有任何指针可达时，靠内容特征把后续碎片
// 猜回来（carving）。
//
// 边界先说清楚，免得用错：
//   - 这里只挑候选，不证明接对了。熵编码流里任意字节都能接任意字节，特征只能否决、
//     不能确认；猜出来的簇一律记进 chainPlan.guessed，写盘时单独标出来。
//   - 已知事实优先于任何打分：目录项给了 DataLength（拼够就停）和 NoFatChain
//     （连续分配，planChain 已经按自增走过了）。打分只在这些事实都断掉之后才上场。
//   - 猜不出来就返回 nil。宁可少给，别给错的 —— 错的字节混在前半段完好的数据里，
//     比少给几簇难发现得多。

// indexEdgeScore 只用索引里的特征给「a → b」这一对打分，不回读磁盘。
//
// 两边都得在扫描时命中过才打得出分；有一边查不到就不打 —— 不值得为了记一条日志
// 把整簇再读一遍，扫描阶段已经读过一次了。
// 打分函数由 Hit 自己按格式分发，这里不用知道簇里装的是 JPEG 还是 MP4。
func indexEdgeScore(stats *ScanStats, a, b uint32) (float64, bool) {
	ha, okA := stats.HitAt(a)
	hb, okB := stats.HitAt(b)
	if !okA || !okB {
		return 0, false
	}
	return ha.ScoreNext(hb), true
}

// logEdgeScore 把「prevCID → nextCID」这一对的关系分打出来。
//
// 凡是判断两簇关系的地方都要走这里。拼接的依据全在这个分数上：不打出来，事后既
// 解释不了为什么接了这一簇，也没法回来调阈值。
//
// why 说明这一对是在哪一步判的（连续自增、窗口搜索、尾巴……），extra 带上那一步
// 自己的上下文。级别是 Debug：每一步都打，走 Info 会刷屏。
func logEdgeScore(logger *slog.Logger, path string, prevCID, nextCID uint32,
	score float64, why string, extra ...any) {
	if logger == nil {
		return
	}
	attrs := []any{
		slog.String("entry", path),
		slog.Uint64("prev", uint64(prevCID)),
		slog.Uint64("next", uint64(nextCID)),
		slog.String("why", why),
		slog.Float64("score", score),
	}
	logger.Debug("cluster edge score", append(attrs, extra...)...)
}

// expectedFormat 按扩展名猜这个文件「按理说」该是什么格式，返回对应的「文件头」分类。
// 猜不出（我们不认识的格式）返回 KindNone。
//
// 只有先有预期，才敢因为内容不像就判失败：卷上还有 txt、pdf 这些我们没做特征识别的
// 格式，它们的第一簇当然对不上任何格式头，不能一并否掉。
func expectedFormat(path string) feature.Kind {
	lower := strings.ToLower(path)
	for _, ext := range []string{".jpg", ".jpeg", ".jpe"} {
		if strings.HasSuffix(lower, ext) {
			return feature.KindJPEGHeader
		}
	}
	for _, ext := range []string{".mp4", ".mov", ".m4v", ".m4a"} {
		if strings.HasSuffix(lower, ext) {
			return feature.KindMP4Header
		}
	}
	return feature.KindNone
}

// checkFirstCluster 校验目录项给的第一簇是不是真属于这个文件。
//
// 目录项只给簇号，不给内容：删除之后那一簇很可能被别的数据写过（位图上仍是空闲，
// 因为新文件后来也被删了）。判据是「扫出来的格式跟名字对得上，且文件头标记就在簇首」——
// 对不上就是被污染了，往下写只会产出一个打不开的假阳性。
//
// 只在有证据时才下判断，其余情况一律放过：
//   - 没扫描索引：不判；
//   - 这一簇没被扫到（超出扫描范围，或被活文件占着）：不知道，不判；
//   - 名字看不出格式（我们不认识的格式）：没有预期，不判。
func checkFirstCluster(stats *ScanStats, state map[uint32]ClusterState,
	entry fsinit.FileEntryItem, cid uint32) error {
	want := expectedFormat(entry.Path)
	if stats == nil || want == feature.KindNone {
		return nil
	}
	if cid > stats.ScannedUpTo || state[cid] == ClusterUsed {
		return nil
	}
	h, ok := stats.HitAt(cid)
	if ok && h.Kind == want && h.IsFileHead() {
		return nil
	}

	fc := &FirstClusterError{Cluster: cid, Kind: feature.KindNone, HeadOffset: -1, Path: entry.Path}
	if ok {
		fc.Kind = h.Kind
		fc.HeadOffset = headOffset(h)
	}
	return fc
}

// headOffset 是「文件头标记在簇内哪一字节」的统一问法：JPEG 看 SOI、MP4 看 ftyp，
// 各格式字段名不同，但报错和日志只需要这一个答案。
func headOffset(h feature.Hit) int {
	switch {
	case h.JPEG != nil:
		return h.JPEG.SOIOffset
	case h.MP4 != nil:
		return h.MP4.FTYPOffset
	default:
		return -1
	}
}

const (
	// carveWindow 是往后找碎片的簇号跨度。固定值，不随文件大小变。
	//
	// 不放大是实测换来的：分配器是一路往后写、中间被别的文件插几簇
	// （实测 461 → 463 → 465 → 466，间隔 1~2 簇），间距由「插入了多少」决定，
	// 跟文件多大没关系。放大窗口只会多招来无关候选，而打分分辨不出它们。
	//
	// 真要是遇到了更大的段间距，日志里的 dist 会一路贴着这个值 —— 到时候再调。
	carveWindow = 256

	// carveMinScore 是接受一个「非连续」候选的最低分。连续的 last+1 不看分 ——
	// 那是分配器的默认行为，比任何打分都可靠。
	carveMinScore = 4.0

	// carveMaxClusters 是单个文件最多猜多少簇。防 DataLength 被写坏（比如 0xFFFFFFFF）
	// 时一路猜到盘尾。
	carveMaxClusters = 4096
)

// carveChain 在链的断点上就地续接后续碎片，返回猜出来的簇号（按接上的顺序）。
//
// 由 planChain 在「FAT 说没有下一簇、但 DataLength 还没喂饱」那一刻调用：断点的
// 上下文（最后一簇是谁、还差多少字节）只有那里最清楚，回到调用方再拼一遍既别扭
// 又容易错。last 是链上最后一个有指针可达的簇，want 是还差的字节数。
func carveChain(parser fsinit.FileSystemParser, stats *ScanStats, state map[uint32]ClusterState,
	claims map[uint32]string, last uint32, want uint64, clusterSize uint64,
	format feature.Kind, logger *slog.Logger, path string) []uint32 {
	if stats == nil || clusterSize == 0 || want == 0 {
		return nil
	}
	return carveFollow(parser, stats, state, claims, last, want, clusterSize,
		format, logger, path)
}

// carveFollow 从 last 往后一簇一簇地把后续碎片接出来。want 是还差多少字节。
// format 是这个文件的目标格式（由第一簇定），用来引导挑选；KindNone 表示不知道。
// logger / path 只用来把每一步的关系分打出来（见 logEdgeScore）。
func carveFollow(parser fsinit.FileSystemParser, stats *ScanStats, state map[uint32]ClusterState,
	claims map[uint32]string, last uint32, want uint64, clusterSize uint64,
	format feature.Kind, logger *slog.Logger, path string) []uint32 {

	// 前一簇的命中信息是打分基准：后面每一簇都要跟它比。
	prev := hitOf(parser, stats, last)
	if prev == nil {
		return nil // 连前一段长什么样都不知道，没有打分依据，别瞎猜
	}

	var guessed []uint32
	used := map[uint32]bool{last: true}

	for want > 0 && len(guessed) < carveMaxClusters {
		// 还差不到一簇的量 —— 这就是文件的最后一簇。它通常只有开头一小截是图像
		// 数据，后面全是尾部空隙（旧数据或 0）：整簇统计起来根本不像 JPEG，进不了
		// 索引。继续按「像不像 JPEG」挑，就会系统性地把文件尾巴砍掉 —— 没有尾巴
		// 就没有 EOI，这份文件直接打不开，前面几簇白拼。
		// 所以最后一簇只要求：紧挨着、能拿。多出来的 slack 解码器一律无视，
		// 少了它文件就是残的。
		if want <= clusterSize {
			// 最后一簇：不假定它紧挨着（可能隔着别的文件），也不要求它认得出格式。
			if tail, ok := pickTail(stats, state, claims, used, last, format); ok {
				guessed = append(guessed, tail)
				// 最后一簇多半统计上认不出格式，关系分打不出来；那就只记下接了谁，
				// 事后能对着簇号回查。
				if score, ok := indexEdgeScore(stats, last, tail); ok {
					logEdgeScore(logger, path, last, tail, score, "tail")
				} else {
					logEdgeScore(logger, path, last, tail, 0, "tail_no_feature")
				}
			}
			break
		}

		next, ok := pickNext(stats, state, claims, used, prev, last, format, logger, path)
		if !ok {
			break // 窗口里没有能接的，到此为止
		}

		used[next] = true
		guessed = append(guessed, next)

		// 这一簇里有结束标记（JPEG 的 EOI）：数据流到此为止，后面全是空隙，不用再找。
		// MP4 没有结束标记，它只能靠下面那条 DataLength 喂没喂饱来判断。
		if h, hit := stats.HitAt(next); hit && h.IsFileEnd() {
			break
		}

		if want <= clusterSize {
			break // 这一簇已经把 DataLength 喂饱
		}
		want -= clusterSize

		last, prev = next, hitOf(parser, stats, next)
		if prev == nil {
			break
		}
	}
	return guessed
}

// pickNext 挑 last 的下一簇。
//
// 三条路，依次退让：
//  1. 连续的 last+1（严格）：分配器的默认行为，比任何打分可靠；
//  2. 窗口里按分挑（严格）：要求候选在索引里、且判成同一格式的后续碎片；
//  3. 格式引导的兜底：见 pickLoose。这一条只给「没有可判定局部规律」的格式开
//     （现在只有 MP4）—— 视频的 mdat 逐簇认不出来，只能靠分配规律往前接。
//
// want 是这个文件该是什么格式（由第一簇定，见 planChain）；KindNone 表示不知道。
func pickNext(stats *ScanStats, state map[uint32]ClusterState, claims map[uint32]string,
	used map[uint32]bool, prev *feature.Hit, last uint32, want feature.Kind,
	logger *slog.Logger, path string) (uint32, bool) {

	// 一、连续的那簇直接收，不看分（分配器的默认行为比打分可靠），但分照样记一笔：
	// 事后要能看出「这一段虽然按连续收了，其实关系分很低」，那才是真断了的地方。
	if cont := last + 1; usable(stats, state, claims, used, cont, want) {
		if score, ok := indexEdgeScore(stats, last, cont); ok {
			logEdgeScore(logger, path, last, cont, score, "contiguous")
		}
		return cont, true
	}

	// 二、候选到扫描索引里找，不去遍历簇号空间：后者绝大多数簇号压根不是候选，空转。
	//
	// FragmentClusters 是扫描时判定为「后续碎片」的簇（各格式合在一起），按簇号升序
	// 排好，二分定位到 last+1，往后取、直到超出 carveWindow 的跨度为止。
	//
	// 只看后面，这是实测换来的：理论上分配器会回头填空隙（后续碎片簇号更小），
	// 但放开双向之后立刻出现了 461 → 143 这种倒着接的错拼 —— 而那个错误候选的
	// 分数（6.25）还比正确的（5.75）高。收益远抵不上代价，先卡死前向。
	payloads := stats.FragmentClusters()
	pos, _ := slices.BinarySearch(payloads, last+1)
	limit := last + carveWindow

	// 谁在前由簇号距离说了算，分数只否决、不排序。
	//
	// 分配器一路往后写、中间被别的文件插几簇（实测 461 → 463 → 465 → 466，间隔
	// 1~2 簇），所以下一片几乎总是最近的那个。而打分的分辨力撑不起排序 —— 实测
	// 错候选 6.25 分反超正确候选 5.75 分 —— 让它排序等于把决定权交给掷骰子。
	// 所以：分数不够的挡掉，剩下的取最近的（payloads 升序，第一个过线的就是最近的）。
	var (
		best       uint32
		bestScore  float64
		considered int
		vetoed     int
	)
	for _, cid := range payloads[pos:] {
		if cid > limit {
			break
		}
		if !usable(stats, state, claims, used, cid, want) {
			continue
		}
		h, _ := stats.HitAt(cid)
		considered++
		// 打分由 Hit 自己按格式分发：JPEG 走 jpeg.ScoreNext，MP4 走 mp4.ScoreNext。
		// 格式对不上时给 0，等于否决（低于 carveMinScore）。
		score := prev.ScoreNext(h)
		if score < carveMinScore {
			vetoed++ // 分数不够：否决，接着看更远的那个
			continue
		}
		best, bestScore = cid, score
		break
	}
	// TODO(贪心不够)：现在每步只挑一个就往下走，一步走错后面全错，也没法回头。
	// 正经做法是遍历成树：每一步保留若干个候选（beam），走不通再回溯到上一个分叉。
	// 难点在剪枝：候选一多就爆炸，得拿解码器之类的硬判据来砍，光靠打分砍不动 ——
	// 实测错候选能比正确候选高出 0.5 分，这分辨力撑不起搜索。
	if best != 0 {
		logEdgeScore(logger, path, last, best, bestScore, "window",
			slog.Int("candidates", considered),
			slog.Int("vetoed", vetoed),
			slog.Uint64("dist", uint64(best-last)))
		return best, true
	}

	// 三、兜底：格式引导，不要求候选自证格式。
	return pickLoose(stats, state, claims, used, last, want, logger, path)
}

// pickTail 往前搜第一个「不是别的东西」的簇，用来接最后一簇。
//
// 最后一簇有两个特点，都得照顾到：
//  1. 认不出格式：它通常只有开头一小截是真实数据，后面全是空隙，统计上不像任何
//     格式（严格路径会把它否掉）。所以这里不要求它自证身份。
//  2. 不一定紧挨着：文件被别的数据插断时，最后一簇可能隔了好几个簇号。
//     实测踩过 —— 链是 …317 → 320，而 318、319 属于别的文件，假定 last+1
//     就把尾巴丢了（8 簇只拼出 7 簇）。
//
// 排除的是「它其实是别的东西」：被活文件占着、已被认领、扫过且判成别种格式、
// 扫过且是某个文件的头。没扫过的簇不作数（连反证都拿不到）。
func pickTail(stats *ScanStats, state map[uint32]ClusterState, claims map[uint32]string,
	used map[uint32]bool, last uint32, want feature.Kind) (uint32, bool) {
	limit := last + carveWindow
	for cid := last + 1; cid <= limit; cid++ {
		if !takeable(state, claims, used, cid) {
			continue
		}
		if cid > stats.ScannedUpTo {
			break
		}
		if h, ok := stats.HitAt(cid); ok {
			if h.IsFileStart() {
				continue
			}
			if want != feature.KindNone && h.Kind != feature.KindNone && h.Kind != want {
				continue
			}
		}
		return cid, true
	}
	return 0, false
}

// pickLoose 是给「逐簇认不出来」的格式留的兜底。
//
// 为什么需要它：视频（MP4 的 mdat）在字节层面没有任何局部规律可依 —— 不像 JPEG
// 那样有「每个 FF 后面必须补 00」这种在每个位置都成立的判据。实测下来，逐簇识别
// 视频只有两种结果：判据一松，全卡 99% 的簇都成 MP4；一紧，一个都认不出来。
// 中间没有稳定区间，说明这件事孤立地看一簇本就不可判定。
//
// 所以这里换依据：不要求候选自证是视频，只排除「它其实是别的东西」——
//   - 被活文件占着：不是；
//   - 已被别的文件认领：不是；
//   - 扫过、且索引里判成别种格式的碎片：不是；
//   - 扫过、且索引里说它是某个文件的头：不是。
//
// 剩下的（扫过但什么都不像）就是要的 —— 视频的 mdat 正是这个样子。
//
// 只给这类格式开，别扩散：JPEG 有强判据，认不出来就是真不是，兜底只会接错。
func pickLoose(stats *ScanStats, state map[uint32]ClusterState, claims map[uint32]string,
	used map[uint32]bool, last uint32, want feature.Kind,
	logger *slog.Logger, path string) (uint32, bool) {
	if want != feature.KindMP4Payload {
		return 0, false
	}

	cid, ok := pickTail(stats, state, claims, used, last, want)
	if ok {
		logEdgeScore(logger, path, last, cid, 0, "loose",
			slog.Uint64("dist", uint64(cid-last)))
	}
	return cid, ok
}

// takeable 判断 cid 在物理上能不能拿：没用过、位图没被占、没被别的文件认领。
// 只管「能不能拿」，不管「像不像 JPEG」—— 中间簇和最后一簇的取舍标准不一样，
// 分开判，别把最后一簇按中间簇的标准卡死。
func takeable(state map[uint32]ClusterState, claims map[uint32]string,
	used map[uint32]bool, cid uint32) bool {
	if used[cid] || state[cid] == ClusterUsed {
		return false
	}
	_, taken := claims[cid]
	return !taken
}

// usable 判断 cid 能不能当「中间簇」候选（严格）：能拿之外还要求它在索引里、
// 且判成目标格式的后续碎片。中间簇整簇都是编码后的数据，判成别的东西就说明它不属于
// 这个文件 —— 这条对中间簇是硬道理。
//
// want 是目标格式（由第一簇定）；KindNone 表示不知道，那就任何格式的碎片都收。
func usable(stats *ScanStats, state map[uint32]ClusterState, claims map[uint32]string,
	used map[uint32]bool, cid uint32, want feature.Kind) bool {
	if !takeable(state, claims, used, cid) {
		return false
	}
	h, ok := stats.HitAt(cid)
	if !ok {
		return false // 索引里没有（没扫到或什么都不像）：交给兜底路径决定
	}
	if h.IsFileStart() {
		return false
	}
	if want == feature.KindNone {
		return h.IsPayload()
	}
	return h.Kind == want
}

// hitOf 取一簇的命中详情：索引里有就直接拿，没有就现读现分一遍。
// 索引里没有通常是「这一簇没被扫到」（比如它不在空闲簇里），现读一次成本可接受 ——
// 每个断链文件最多多读这一簇。
//
// 返回的是整条 Hit 而不是某个格式的特征：打分由 Hit 自己按格式分发，
// 这函数不用知道簇里装的是什么。
func hitOf(parser fsinit.FileSystemParser, stats *ScanStats, cid uint32) *feature.Hit {
	if h, ok := stats.HitAt(cid); ok {
		return &h
	}
	data, err := parser.ReadCluster(cid)
	if err != nil {
		return nil
	}
	// 认不出格式的也照返回：兜底路径要的就是「扫过但什么都不像」的簇
	// （视频的 mdat 正是这样），由调用方决定怎么处理。
	h := feature.Classify(cid, data)
	return &h
}
