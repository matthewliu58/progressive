package recovery

import (
	"log/slog"
	"progrescarve/internal/feature"
	"slices"
	"sync"

	fsinit "progrescarve/internal/fs/finit"
)

// ScanStats 是空闲簇扫描的统计和结果索引。I/O 计数归恢复层，分类词表在 feature 包。
//
// Hits 和 Index 是同一批命中的两种看法：Hits 按类别遍历（日志、候选集），
// Index 按簇号反查（续接时要判断某一簇能不能用）。两者都只记簇号和特征，
// 不保存原始簇数据 —— 整卡空闲簇上百万，留住数据就是几十 GB，要用再按簇号回读。
type ScanStats struct {
	Scanned        int                       // 实际扫了的簇数（空闲 + 归属不明）
	ScannedUnknown int                       // 其中有多少是 ClusterUnknown（位图说占了却没人认领）
	ReadFailed     int                       // 读不出来、没参与统计的簇
	Hits           map[feature.Kind][]uint32 // 各分类命中的簇号；feature.KindNone 不计入
	Index          map[uint32]feature.Hit    // 簇号 → 命中详情，续接时按簇号反查

	// ScannedUpTo 是派发出去读过的最大簇号。
	//
	// 有它才能区分两种「索引里查不到」：扫过但什么都不像（这一簇已被污染）对比
	// 压根没扫到（不作判断）。派发是按簇号升序走的，所以「没被活文件占着且
	// 簇号 ≤ 它」= 读过。
	ScannedUpTo uint32

	// fragments 是所有格式的碎片候选簇号（不含文件头），按升序合并好，
	// 由 FragmentClusters 读出。用 Fragments 建它，不对外暴露写入。
	fragments []uint32
}

// fragmentKinds 是「可以接在别人后面」的那些分类。文件头不在此列 ——
// 它是某个文件的第一簇，只能当起点。
var fragmentKinds = []feature.Kind{feature.KindJPEGPayload, feature.KindMP4Payload}

// FragmentClusters 返回所有格式的碎片候选簇号，按簇号升序合并好。
//
// 分格式存（Hits）是为了日志按类别报数；合起来存是为了搜索时一次扫过所有格式 ——
// carve 在里面二分定位窗口，不必知道哪一簇是 JPEG 哪一簇是 MP4。
func (s ScanStats) FragmentClusters() []uint32 {
	return s.fragments
}

// HitAt 按簇号取命中详情。没命中的簇不在索引里（KindNone 不入表），ok 为 false。
func (s ScanStats) HitAt(cid uint32) (feature.Hit, bool) {
	h, ok := s.Index[cid]
	return h, ok
}

// Payloads 返回按簇号升序排好的「JPEG 后续碎片」候选，不含文件头。
// 续接时在它上面按簇号二分取窗口。
func (s ScanStats) Payloads() []uint32 {
	return s.Hits[feature.KindJPEGPayload]
}

// jpegKinds 是日志里要报告的 JPEG 分类：每种格式各报各的，免得日志字段名乱掉。
// 未分类（KindNone）本来就不进 Hits。
var jpegKinds = []feature.Kind{feature.KindJPEGHeader, feature.KindJPEGPayload}

// mp4Kinds 是日志里要报告的 MP4 分类。
var mp4Kinds = []feature.Kind{feature.KindMP4Header, feature.KindMP4Payload}

// hitAttrs 把各类命中数摊成日志字段。字段固定、零也照打，日志行可 diff。
// 加格式时把它的分类加进 mp4Kinds 那样的清单里，这里不用动。
func hitAttrs(hits map[feature.Kind][]uint32) []any {
	kinds := make([]feature.Kind, 0, len(jpegKinds)+len(mp4Kinds))
	kinds = append(kinds, jpegKinds...)
	kinds = append(kinds, mp4Kinds...)

	attrs := make([]any, 0, len(kinds))
	for _, k := range kinds {
		attrs = append(attrs, slog.Int(k.String(), len(hits[k])))
	}
	return attrs
}

// scanLimit 是扫描上限（按派发出去读的空闲簇数计）：整卡空闲簇上百万，全扫一遍
// 要很久；先扫这么多个空闲簇看分类结果，验证够了再放开（math.MaxInt）。
const scanLimit = 100_000

// scanProgressEvery 是进度日志的间隔，按已扫空闲簇数计。
const scanProgressEvery = 10_000

// scanWorkers 是并发读簇的 goroutine 数。ReadCluster 走 ReadAt：位置无关、无共享
// 文件偏移，并发调用安全，不用加锁。
// 固定档位，不按介质调：并发读不是固态的专利，HDD 照样能跑，只是随机寻道开销大、
// 提速有限甚至持平，慢一点而已。为判断介质多一套代码不值得，慢就慢点。
const scanWorkers = 8

// Scan 把「没有活文件占着」的簇读出来，按 feature.Classify 分类计数，
// 派满 scanLimit 个簇为止。
//
// 扫哪些簇：ClusterFree（位图未占用）和 ClusterUnknown（位图说占了，但没有活文件
// 认领）都要扫。后者是实测逼出来的 —— 删文件时 FAT 清了、位图却未必跟着清，
// 于是那些簇成了「标着已分配却没人认」的状态，里面装的正是被删文件的残骸。
// 只扫 ClusterFree 会把它们整片跳过（实测一个已删 MP4 只有第一簇是 Free，
// 后续七簇全是 Unknown，一个都没被扫到）。
// ClusterUsed 不扫：那是活文件正在用的数据，拿来拼被删文件只会拼错。
//
// 只统计、不写盘。单簇读失败只跳过不报错；每 scanProgressEvery 个簇报一次进度。
func Scan(parser fsinit.FileSystemParser, state map[uint32]ClusterState, pre string,
	logger *slog.Logger) ScanStats {
	_, firstCid, totalCid := parser.ClusterHeapRange()
	lastCid := firstCid + totalCid

	var (
		mu  sync.Mutex // 守 sum：Hits/Index 是 map，多 worker 并发写会炸
		sum = ScanStats{
			Hits:  make(map[feature.Kind][]uint32),
			Index: make(map[uint32]feature.Hit),
		}
	)

	jobs := make(chan uint32, scanWorkers*2)
	var wg sync.WaitGroup
	for range scanWorkers {
		wg.Go(func() {
			for cid := range jobs {
				data, err := parser.ReadCluster(cid)
				if err != nil {
					mu.Lock()
					sum.ReadFailed++
					mu.Unlock()
					continue
				}
				h := feature.Classify(cid, data)

				if h.Kind != feature.KindNone {
					logger.Debug("scan cluster hit",
						slog.String("pre", pre),
						slog.Uint64("cluster", uint64(cid)),
						slog.String("kind", h.Kind.String()))
				}

				mu.Lock()
				sum.Scanned++
				if state[cid] == ClusterUnknown {
					sum.ScannedUnknown++
				}
				if h.Kind != feature.KindNone {
					sum.Hits[h.Kind] = append(sum.Hits[h.Kind], h.Cluster)
					sum.Index[h.Cluster] = h
				}
				if sum.Scanned%scanProgressEvery == 0 {
					logger.Info("scan free clusters progress",
						append([]any{slog.String("pre", pre), slog.Int("scanned", sum.Scanned)},
							hitAttrs(sum.Hits)...)...)
				}
				mu.Unlock()
			}
		})
	}

	// 派发：只发「没有活文件占着」的簇，发够上限就停，剩下的簇不再派活。
	scheduled := 0
	lastDispatched := uint32(0) // 派出去的最后一簇：扫描覆盖到哪，靠它记
dispatch:
	for cid := firstCid; cid < lastCid; cid++ {
		if state[cid] == ClusterUsed {
			continue
		}
		jobs <- cid
		scheduled++
		lastDispatched = cid
		if scheduled >= scanLimit {
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()

	// 派发在主 goroutine，写 sum 的其它字段都上了锁；这个等 worker 全部收工后再写，
	// 省得为了一个字段再多一把锁 —— 这时候已经没人跟主 goroutine 抢了。
	sum.ScannedUpTo = lastDispatched

	// 并发完成顺序不固定，簇号列表排回来，进度日志和最终结果才可 diff。
	for _, list := range sum.Hits {
		slices.Sort(list)
	}

	// 各格式的碎片候选合成一张有序表：每一类自己已经排好序，归并一下即可。
	// carve 只在这张表上二分取窗口，不关心簇里装的是哪种格式。
	for _, k := range fragmentKinds {
		sum.fragments = append(sum.fragments, sum.Hits[k]...)
	}
	slices.Sort(sum.fragments)

	if scheduled >= scanLimit {
		// 说清楚是「到量了」不是「扫完了」，别让人以为空闲簇只有这么多。
		logger.Info("scan limit reached",
			append([]any{slog.String("pre", pre), slog.Int("scanned", sum.Scanned)},
				hitAttrs(sum.Hits)...)...)
	}
	return sum
}
