package recovery

import (
	"log/slog"
	"progrescarve/internal/feature"
	"slices"
	"sync"

	fsinit "progrescarve/internal/fs/finit"
)

// ScanStats 是空闲簇扫描的运行统计。I/O 计数归恢复层；分类词表在 feature 包。
// Hits 记簇号而不是只记个数：碎片重组阶段要按簇号重读命中簇，个数用 len 就有。
type ScanStats struct {
	Scanned    int                       // 实际扫了的空闲簇数
	ReadFailed int                       // 读不出来、没参与统计的簇
	Hits       map[feature.Kind][]uint32 // 各分类命中的簇号；feature.KindNone 不计入
}

// jpegKinds 是日志里要报告的分类：现在只关心 JPEG；未分类（KindNone）本来就不进
// Hits，其他分类加了也先不在日志里报。
var jpegKinds = []feature.Kind{feature.KindJPEGHeader, feature.KindJPEGPayload}

// hitAttrs 把 JPEG 各类命中数摊成日志字段。字段固定、零也照打，日志行可 diff。
func hitAttrs(hits map[feature.Kind][]uint32) []any {
	attrs := make([]any, 0, len(jpegKinds))
	for _, k := range jpegKinds {
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

// Scan 把位图未占用的簇读出来，按 feature.Classify 分类计数，派满 scanLimit 个空闲簇为止。
// 只统计、不写盘。单簇读失败只跳过不报错；每 scanProgressEvery 个空闲簇报一次进度。
func Scan(parser fsinit.FileSystemParser, state map[uint32]ClusterState, pre string, logger *slog.Logger) ScanStats {
	_, firstCid, totalCid := parser.ClusterHeapRange()
	lastCid := firstCid + totalCid

	var (
		mu  sync.Mutex // 守 sum：Hits 是 map，多 worker 并发写会炸
		sum = ScanStats{Hits: make(map[feature.Kind][]uint32)}
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
				if h.Kind != feature.KindNone {
					sum.Hits[h.Kind] = append(sum.Hits[h.Kind], h.Cluster)
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

	// 派发：只发空闲簇，发够上限就停，剩下的簇不再派活。
	scheduled := 0
dispatch:
	for cid := firstCid; cid < lastCid; cid++ {
		if state[cid] != ClusterFree {
			continue
		}
		jobs <- cid
		scheduled++
		if scheduled >= scanLimit {
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()

	// 并发完成顺序不固定，簇号列表排回来，进度日志和最终结果才可 diff。
	for _, list := range sum.Hits {
		slices.Sort(list)
	}

	if scheduled >= scanLimit {
		// 说清楚是「到量了」不是「扫完了」，别让人以为空闲簇只有这么多。
		logger.Info("scan limit reached",
			append([]any{slog.String("pre", pre), slog.Int("scanned", sum.Scanned)},
				hitAttrs(sum.Hits)...)...)
	}
	return sum
}
