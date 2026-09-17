package recovery

import (
	"log/slog"
	"slices"
	"strings"

	"progrescarve/internal/feature"
	"progrescarve/internal/feature/jpeg"

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
// 两边都得在扫描时命中过（也就是都是 JPEG 候选）才打得出分；有一边查不到就不打 ——
// 不值得为了记一条日志把整簇再读一遍，扫描阶段已经读过一次了。
func indexEdgeScore(stats *ScanStats, a, b uint32) (float64, bool) {
	ha, okA := stats.HitAt(a)
	hb, okB := stats.HitAt(b)
	if !okA || !okB || ha.JPEG == nil || hb.JPEG == nil {
		return 0, false
	}
	return jpeg.ScoreNext(*ha.JPEG, *hb.JPEG), true
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

// expectsJPEG 按扩展名判断这个文件「按理说」该是 JPEG。
//
// 只有先有预期，才敢因为内容不像就判失败：卷上还有 mp4、txt 这些我们不认识的格式，
// 它们的第一簇当然不是 JPEG 头，不能一并否掉。
func expectsJPEG(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range []string{".jpg", ".jpeg", ".jpe"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// checkFirstCluster 校验目录项给的第一簇是不是真属于这个文件。
//
// 目录项只给簇号，不给内容：删除之后那一簇很可能被别的数据写过（位图上仍是空闲，
// 因为新文件后来也被删了）。扫过、且名字说是 JPEG、但簇首不是 SOI —— 那就是被污染了，
// 往下写只会产出一个打不开的假阳性。
//
// 只在有证据时才下判断，其余情况一律放过：
//   - 没扫描索引：不判；
//   - 这一簇没被扫到（超出扫描范围，或位图不是空闲）：不知道，不判；
//   - 名字看不出是 JPEG：没有预期，不判。
func checkFirstCluster(stats *ScanStats, state map[uint32]ClusterState,
	entry fsinit.FileEntryItem, cid uint32) error {
	if stats == nil || !expectsJPEG(entry.Path) {
		return nil
	}
	if cid > stats.ScannedUpTo || state[cid] != ClusterFree {
		return nil
	}
	h, ok := stats.HitAt(cid)
	if ok && h.IsJPEGHead() {
		return nil
	}

	fc := &FirstClusterError{Cluster: cid, Kind: feature.KindNone, SOIOffset: -1, Path: entry.Path}
	if ok {
		fc.Kind = h.Kind
		if h.JPEG != nil {
			fc.SOIOffset = h.JPEG.SOIOffset
		}
	}
	return fc
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
	logger *slog.Logger, path string) []uint32 {
	if stats == nil || clusterSize == 0 || want == 0 {
		return nil
	}
	return carveFollow(parser, stats, state, claims, last, want, clusterSize, logger, path)
}

// carveFollow 从 last 往后一簇一簇地把后续碎片接出来。want 是还差多少字节。
// logger / path 只用来把每一步的关系分打出来（见 logEdgeScore）。
func carveFollow(parser fsinit.FileSystemParser, stats *ScanStats, state map[uint32]ClusterState,
	claims map[uint32]string, last uint32, want uint64, clusterSize uint64,
	logger *slog.Logger, path string) []uint32 {

	// 前一簇的特征是打分基准：后面每一簇都要跟它比。
	prev := featureOf(parser, stats, last)
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
			if tail := last + 1; tailOK(stats, state, claims, used, tail) {
				guessed = append(guessed, tail)
				// 最后一簇通常统计上不像 JPEG，索引里没有它，多半打不出关系分；
				// 那就只记下接了谁，事后能对着簇号回查。
				if score, ok := indexEdgeScore(stats, last, tail); ok {
					logEdgeScore(logger, path, last, tail, score, "tail")
				} else {
					logEdgeScore(logger, path, last, tail, 0, "tail_no_feature")
				}
			}
			break
		}

		next, ok := pickNext(stats, state, claims, used, prev, last, logger, path)
		if !ok {
			break // 窗口里没有能接的，到此为止
		}

		used[next] = true
		guessed = append(guessed, next)

		// 这一簇里有 EOI：文件数据流到此结束，后面全是 padding，不用再找。
		if h, hit := stats.HitAt(next); hit && h.IsJPEGEnd() {
			break
		}

		if want <= clusterSize {
			break // 这一簇已经把 DataLength 喂饱
		}
		want -= clusterSize

		last, prev = next, featureOf(parser, stats, next)
		if prev == nil {
			break
		}
	}
	return guessed
}

// pickNext 挑 last 的下一簇。
//
// 先试连续的 last+1：exFAT 分配器默认连续分配，这一种的命中率远高于任何打分，
// 而且它是「分配器的行为」，不是统计巧合。连续的那簇不能用，才去候选里按分挑。
func pickNext(stats *ScanStats, state map[uint32]ClusterState, claims map[uint32]string,
	used map[uint32]bool, prev *jpeg.Feature, last uint32,
	logger *slog.Logger, path string) (uint32, bool) {

	// 连续的那簇直接收，不看分（分配器的默认行为比打分可靠），但分照样记一笔：
	// 事后要能看出「这一段虽然按连续收了，其实关系分很低」，那才是真断了的地方。
	if cont := last + 1; usable(stats, state, claims, used, cont) {
		if score, ok := indexEdgeScore(stats, last, cont); ok {
			logEdgeScore(logger, path, last, cont, score, "contiguous")
		}
		return cont, true
	}

	// 候选到扫描索引里找，不去遍历簇号空间：后者绝大多数簇号压根不是候选，空转。
	//
	// Payloads 是扫描时判定为「JPEG 后续碎片」的簇，按簇号升序排好，二分定位到
	// last+1，往后取、直到超出 carveWindow 的跨度为止。
	//
	// 只看后面，这是实测换来的：理论上分配器会回头填空隙（后续碎片簇号更小），
	// 但放开双向之后立刻出现了 461 → 143 这种倒着接的错拼 —— 而那个错误候选的
	// 分数（6.25）还比正确的（5.75）高。收益远抵不上代价，先卡死前向。
	payloads := stats.Payloads()
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
		if !usable(stats, state, claims, used, cid) {
			continue
		}
		h, _ := stats.HitAt(cid)
		considered++
		score := jpeg.ScoreNext(*prev, *h.JPEG)
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
	if best == 0 {
		return 0, false
	}
	logEdgeScore(logger, path, last, best, bestScore, "window",
		slog.Int("candidates", considered),
		slog.Int("vetoed", vetoed),
		slog.Uint64("dist", uint64(best-last)))
	return best, true
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

// usable 判断 cid 能不能当「中间簇」候选：能拿之外还要求它像 JPEG。
// 中间簇整簇都是熵编码数据，不像就说明它不属于这个文件 —— 这条对中间簇是硬道理。
func usable(stats *ScanStats, state map[uint32]ClusterState, claims map[uint32]string,
	used map[uint32]bool, cid uint32) bool {
	if !takeable(state, claims, used, cid) {
		return false
	}
	h, ok := stats.HitAt(cid)
	return ok && h.IsJPEG() && !h.IsJPEGStart()
}

// tailOK 判断 cid 能不能当「最后一簇」：能拿就行，不要求像 JPEG。
// 唯一的额外否决是里面别有 SOI —— 那是另一个文件的开头，不是我们的尾巴。
func tailOK(stats *ScanStats, state map[uint32]ClusterState, claims map[uint32]string,
	used map[uint32]bool, cid uint32) bool {
	if !takeable(state, claims, used, cid) {
		return false
	}
	if h, ok := stats.HitAt(cid); ok && h.IsJPEGStart() {
		return false
	}
	return true
}

// featureOf 取一簇的 JPEG 特征：索引里有就直接拿，没有就现读现扫。
// 索引里没有通常是「这一簇没被扫到」（比如它不在空闲簇里），现读一次成本可接受 ——
// 每个断链文件最多多读这一簇。
func featureOf(parser fsinit.FileSystemParser, stats *ScanStats, cid uint32) *jpeg.Feature {
	if h, ok := stats.HitAt(cid); ok && h.JPEG != nil {
		return h.JPEG
	}
	data, err := parser.ReadCluster(cid)
	if err != nil {
		return nil
	}
	f := jpeg.Scan(data)
	return &f
}
