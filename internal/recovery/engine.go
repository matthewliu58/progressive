package recovery

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"progrescarve/internal/fs"
	fsinit "progrescarve/internal/fs/finit"
)

// ClusterState 是簇堆里单个簇的占用状态。
type ClusterState uint8

const (
	// ClusterFree 空闲：分配位图未占用，也没有任何未删除文件经过。
	ClusterFree ClusterState = 0
	// ClusterUsed 确定占用：被未删除文件的簇链覆盖，或者属于文件系统自身的
	// 公共信息簇（分配位图、根目录……）。这簇上的旧数据一定已被覆盖。
	ClusterUsed ClusterState = 1
	// ClusterUnknown 其他：分配位图显示已占用，但既不属于任何未删除文件的
	// 簇链，也不是已知的公共信息簇 —— 归属不明，可能还残留着旧数据。
	ClusterUnknown ClusterState = 2
)

// BuildClusterState 遍历所有「未删除」的目录条目，沿各自的簇链走一遍，得到
// 一份「簇号 → 占用状态」的表：0 空闲 / 1 确定占用 / 2 其他。
//
// 用途：恢复「已删除但目录条目还在」的文件时，拿它原来的簇号去表里查 ——
// 查到 ClusterUsed 就说明这簇已经被未删除的文件或文件系统自身占住了，
// 原内容确定被覆盖，不必再尝试恢复。
//
// 只读元数据（分配位图 + FAT + 目录条目），不读任何文件内容。
func BuildClusterState(parser fsinit.FileSystemParser, logger *slog.Logger, pre string) (map[uint32]ClusterState, error) {
	clusterSize, firstCid, totalCid := parser.ClusterHeapRange()
	if totalCid == 0 {
		return nil, errors.New("cluster heap is empty")
	}
	lastCid := firstCid + totalCid // 合法簇号区间是 [firstCid, lastCid)

	// 单条簇链最多允许走多少簇：有 DataLength 就按它算，否则以整个簇堆为上限。
	// 主要是防止 FAT 链成环时无限循环。
	chainLimit := func(dataLength uint64) uint64 {
		if clusterSize == 0 || dataLength == 0 {
			return uint64(totalCid)
		}
		n := (dataLength + clusterSize - 1) / clusterSize
		if n == 0 || n > uint64(totalCid) {
			return uint64(totalCid)
		}
		return n
	}

	state := make(map[uint32]ClusterState, totalCid)

	// markChain 沿一条簇链往下走，把经过的簇标成确定占用。
	markChain := func(e fsinit.FileEntryItem) {
		cid := e.FirstCluster
		limit := chainLimit(e.DataLength)
		seen := make(map[uint32]struct{}, limit)

		for i := uint64(0); i < limit; i++ {
			if cid < firstCid || cid >= lastCid {
				return // 簇号越界，链坏在这里
			}
			if _, dup := seen[cid]; dup {
				return // FAT 链成环
			}
			seen[cid] = struct{}{}
			state[cid] = ClusterUsed

			if e.NoFatChain {
				cid++ // 连续分配，簇号直接递增
				continue
			}
			_, next, err := parser.GetClusterFSInfo(cid)
			if err != nil || next < firstCid || next >= lastCid {
				return // 链尾，或该簇的信息读不出来
			}
			cid = next
		}
	}

	// 1) 文件系统自身的公共信息簇：不承载文件内容，但确实占着簇。
	for _, cid := range parser.SystemClusters() {
		if cid >= firstCid && cid < lastCid {
			state[cid] = ClusterUsed
		}
	}

	// 2) 所有未删除的条目，沿簇链逐个标记。已删除的跳过 —— 它们的簇正是
	//    后面要判断的对象。
	entries, err := parser.ListAllFileEntries()
	if err != nil {
		return nil, fmt.Errorf("list file entries: %w", err)
	}
	liveEntries := 0
	for i := range entries {
		e := entries[i]
		if e.IsDeleted || e.FirstCluster < firstCid {
			continue
		}
		markChain(e)
		liveEntries++
	}

	// 3) 剩下的簇，按分配位图区分「空闲」和「其他」。
	var freeCount, unknownCount int
	for cid := firstCid; cid < lastCid; cid++ {
		if _, done := state[cid]; done {
			continue
		}
		alloc, _, err := parser.GetClusterFSInfo(cid)
		if err != nil {
			continue
		}
		if alloc {
			state[cid] = ClusterUnknown
			unknownCount++
		} else {
			state[cid] = ClusterFree
			freeCount++
		}
	}

	logger.Info("cluster state built",
		slog.String("pre", pre),
		slog.Int("live_entries", liveEntries),
		slog.Int("used", len(state)-freeCount-unknownCount),
		slog.Int("unknown", unknownCount),
		slog.Int("free", freeCount),
		slog.Int("total", int(totalCid)),
	)
	return state, nil
}

func Engine(path string, pre string, logger *slog.Logger) error {

	slog.Info("recovery start", slog.String("pre", pre), slog.String("path", path))

	f, err := os.Open(path)
	if err != nil {
		logger.Error("open err", slog.String("pre", pre),
			slog.Any("err", err), slog.String("path", path))
		return err
	}
	defer f.Close()

	parser, err := fs.DetectFileSystem(f, logger, pre)
	if err != nil {
		logger.Error("detect fs err", slog.String("pre", pre),
			slog.Any("err", err), slog.String("path", path))
		return err
	}

	err = parser.Load(f)
	if err != nil {
		logger.Error("parser load err", slog.String("pre", pre),
			slog.Any("err", err), slog.String("path", path))
		return err
	}

	parser.DebugPrintMeta()

	// 把未删除文件占用的簇全部记下来，后续恢复已删除文件时用它判断
	// 原簇是否已被覆盖。
	if _, err := BuildClusterState(parser, logger, pre); err != nil {
		logger.Error("build cluster state err", slog.String("pre", pre),
			slog.Any("err", err), slog.String("path", path))
		return err
	}

	logger.Info("recovery end", slog.String("pre", pre), slog.String("path", path))
	return nil
}
