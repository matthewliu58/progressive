package recovery

import (
	"log/slog"
	"progrescarve/internal/feature"

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

// scanLimit 是扫描上限：整卡空闲簇上百万，全扫一遍要很久；先扫 1 万个空闲簇
// 看分类结果，验证够了再放开（math.MaxInt）。
const scanLimit = 10_000

// scanProgressEvery 是进度日志的间隔，按已扫空闲簇数计。
const scanProgressEvery = 1_000

// Scan 把位图未占用的簇逐个读出来，按 feature.Classify 分类计数，扫满 scanLimit 个空闲簇为止。
// 只统计、不写盘。单簇读失败只跳过不报错；每 scanProgressEvery 个空闲簇报一次进度。
func Scan(parser fsinit.FileSystemParser, state map[uint32]ClusterState, pre string, logger *slog.Logger) ScanStats {
	_, firstCid, totalCid := parser.ClusterHeapRange()
	lastCid := firstCid + totalCid

	sum := ScanStats{Hits: make(map[feature.Kind][]uint32)}
	for cid := firstCid; cid < lastCid; cid++ {
		if state[cid] != ClusterFree {
			continue
		}
		data, err := parser.ReadCluster(cid)
		if err != nil {
			sum.ReadFailed++
			continue
		}
		sum.Scanned++

		h := feature.Classify(cid, data)
		if h.Kind != feature.KindNone {
			sum.Hits[h.Kind] = append(sum.Hits[h.Kind], h.Cluster)
		}

		// 扫满上限就停，并说清楚是「到量了」不是「扫完了」，别让人以为空闲簇只有这么多。
		if sum.Scanned >= scanLimit {
			logger.Info("scan limit reached",
				append([]any{slog.String("pre", pre), slog.Int("scanned", sum.Scanned)},
					hitAttrs(sum.Hits)...)...)
			return sum
		}

		// 每簇一条 debug：终端是 Info 级看不见、不刷屏；文件里全程留痕，
		// 扫描卡住时看最后一条就知道走到哪个簇了。
		//logger.Debug("scan cluster",
		//	slog.String("pre", pre),
		//	slog.Uint64("cluster", uint64(cid)),
		//	slog.String("kind", h.Kind.String()))

		if sum.Scanned%scanProgressEvery == 0 {
			logger.Info("scan free clusters progress",
				append([]any{slog.String("pre", pre), slog.Int("scanned", sum.Scanned)},
					hitAttrs(sum.Hits)...)...)
		}
	}
	return sum
}
