package recovery

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"progrescarve/internal/fs"
	fsinit "progrescarve/internal/fs/finit"
	"strings"
)

// ClusterState 是簇堆里单个簇的占用状态。
type ClusterState uint8

const (
	// ClusterFree 空闲：位图未占用，也没有未删除文件经过。
	ClusterFree ClusterState = 0
	// ClusterUsed 确定占用：被未删除文件的簇链覆盖，或属于文件系统自身（位图、根目录等）。旧数据必已被覆盖。
	ClusterUsed ClusterState = 1
	// ClusterUnknown 位图说占用了，但既不属于任何未删除文件，也不是系统簇 —— 归属不明，可能还有旧数据。
	ClusterUnknown ClusterState = 2
)

// systemOwner 是「占用者其实是文件系统自身」时在 ClusterOwner 里记下的名字。
const systemOwner = "<filesystem>"

// ClusterClaim 说明某个簇被谁占着，以及它位于占用者簇链的第几环。
// 只记路径对不上号：活文件的簇是散着分配的，得知道链头在哪。
type ClusterClaim struct {
	Entry string // 占用者：未删除条目的路径，或 systemOwner
	Head  uint32 // 占用者簇链的链头；SystemClusters 没有链，记 0
	Link  uint32 // 本簇是链上第几环，从 1 开始；没有链的记 0
}

// ClusterOwner 记录 ClusterUsed 的簇被谁占着：簇号 → 占用者。只用于把失败原因说清楚。
type ClusterOwner map[uint32]ClusterClaim

// OverwrittenError 表示待恢复文件链上的某个簇已被未删除条目占住。
// 单独成类型是为了让调用方结构化地取出「哪个簇、被谁占了」，不用去抠错误字符串。
type OverwrittenError struct {
	Cluster   uint32 // 撞上的簇号
	Owner     string // 占用者：未删除条目的路径，或 systemOwner
	OwnerHead uint32 // 占用者簇链的链头，0 表示没有链
	OwnerLink uint32 // 本簇在占用者链上的序号，0 表示没有链
	Path      string // 待恢复的（已删除）条目路径
}

func (e *OverwrittenError) Error() string {
	where := ""
	// 补上链头和链上序号，否则只看 first_cluster 会以为占用者报错了。
	if e.OwnerLink > 0 {
		where = fmt.Sprintf(" (chain head %d, link %d)", e.OwnerHead, e.OwnerLink)
	}
	return fmt.Sprintf("cluster %d is in use by %q%s, %q was overwritten", e.Cluster, e.Owner, where, e.Path)
}

// ShortChainError 表示已删除条目的簇链在 DataLength 之前就停住了：目录项说还有一截，
// FAT 却说没有下一簇。成因有两种，靠 LastFatNext 区分，别混为一谈：
//   - 0：这一簇已被释放（删文件时链上的 FAT 项被清空）—— 旧数据通常还在盘上；
//   - 0x0FFFFFFF：真正的链尾 EOC（被装载时的 28 位掩码打成越界值）—— 目录项的
//     DataLength 与链本身就不自洽，条目不可尽信。
//
// 两种成因下前段都是完好的：后半段没有任何指针可达，但前段扔了就真没了。
type ShortChainError struct {
	Shortfall   uint64 // 还差多少字节没凑够
	Want        uint64 // 目录项声明的 DataLength
	Got         uint64 // 已走到的簇合计能覆盖多少字节
	LastCluster uint32 // 链停在哪一簇
	LastFatNext uint32 // 该簇 FAT 项的原始值（装载时已掩码）：0=已释放，0x0FFFFFFF=EOC
	Path        string // 待恢复的（已删除）条目路径
}

func (e *ShortChainError) Error() string {
	why := "chain end (EOC)"
	if e.LastFatNext == 0 {
		why = "cluster released"
	}
	return fmt.Sprintf("fat chain stopped at cluster %d (%s): got %d bytes, %d bytes short of DataLength(%d)",
		e.LastCluster, why, e.Got, e.Shortfall, e.Want)
}

// RestoreResult 说明一次恢复实际捞回了多少。
//
// Truncated 必须单独记：断点之后的簇已经没有任何指针可达，而残缺文件的大小是完全
// 合法的 —— 只看 dst 大小分不出「完整」和「只剩前半段」。
type RestoreResult struct {
	Bytes     uint64            // 实际写入 dst 的字节数
	Clusters  int               // 恢复出的簇数
	Truncated bool              // true：只恢复了前半段
	Stop      *OverwrittenError // 断在哪、被谁占了；非「被占」截断时为 nil
	Short     *ShortChainError  // 链在长度前就断了时的详情；否则 nil
}

// BuildClusterState 遍历所有未删除的条目，沿簇链走一遍，得到「簇号 → 占用状态」表：
// 恢复已删除文件时用它判断原来的簇是否已被覆盖。只读元数据，不读文件内容。
func BuildClusterState(parser fsinit.FileSystemParser, logger *slog.Logger, pre string) (map[uint32]ClusterState, ClusterOwner, error) {
	clusterSize, firstCid, totalCid := parser.ClusterHeapRange()
	if totalCid == 0 {
		return nil, nil, errors.New("cluster heap is empty")
	}
	lastCid := firstCid + totalCid // 合法簇号区间是 [firstCid, lastCid)

	// 单条链最多走多少簇，防止 FAT 成环时无限循环。
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
	owners := make(ClusterOwner, totalCid)

	// markChain 沿一条簇链往下走，把经过的簇标成确定占用。
	markChain := func(e fsinit.FileEntryItem) {
		cid := e.FirstCluster
		limit := chainLimit(e.DataLength)
		seen := make(map[uint32]struct{}, limit)

		for i := range limit {
			if cid < firstCid || cid >= lastCid {
				return // 簇号越界，链坏在这里
			}
			if _, dup := seen[cid]; dup {
				return // FAT 链成环
			}
			seen[cid] = struct{}{}
			state[cid] = ClusterUsed
			// 一个簇被多个活文件声明是卷本身有问题，先到先得。
			if _, taken := owners[cid]; !taken {
				owners[cid] = ClusterClaim{Entry: e.Path, Head: e.FirstCluster, Link: uint32(i + 1)}
			}

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
			owners[cid] = ClusterClaim{Entry: systemOwner} // 系统簇不属于任何文件，没有链头和序号
		}
	}

	// 2) 未删除的条目，沿簇链逐个标记。已删除的跳过 —— 它们的簇正是要判断的对象。
	entries, err := parser.ListAllFileEntries()
	if err != nil {
		return nil, nil, fmt.Errorf("list file entries: %w", err)
	}
	liveEntries, deletedEntries := 0, 0
	for i := range entries {
		e := entries[i]
		if e.IsDeleted {
			deletedEntries++
			continue
		}
		if e.FirstCluster < firstCid {
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

	if logger != nil {
		logger.Info("cluster state built",
			slog.String("pre", pre),
			slog.Int("live_entries", liveEntries),
			slog.Int("deleted_entries", deletedEntries),
			slog.Int("used", len(state)-freeCount-unknownCount),
			slog.Int("unknown", unknownCount),
			slog.Int("free", freeCount),
			slog.Int("total", int(totalCid)),
		)
	}
	return state, owners, nil
}

// chainPlan 是沿已删除条目的簇链走一遍的结论，只读元数据，不碰 dst。
//
// chain 恒为「所有还能确定指向的簇」。两种断点都只影响它之后的簇，两者最多有一个非 nil。
type chainPlan struct {
	chain []uint32          // 断点之前、可恢复的簇，按链上顺序
	stop  *OverwrittenError // 非 nil：撞上活文件，之后的簇没有任何指针可达
	short *ShortChainError  // 非 nil：FAT 在 DataLength 之前就没了下一簇
}

// planChain 沿链走一遍并校验。两种「走不到头」都只截断、不报错 —— 断点之前的数据是完好的，
// 扔掉也换不来后半段，所以一律交给调用方写出：
//   - 撞上被占用的簇（活文件）：stop 记下占用者；
//   - FAT 说没有下一簇、但长度还没喂饱：short 记下断在哪一簇、FAT 项是什么值。
//
// 只有无法解释的元数据错误（簇号越界、FAT 成环、FAT 读失败）才返回 error。
func planChain(parser fsinit.FileSystemParser, entry fsinit.FileEntryItem, state map[uint32]ClusterState, owners ClusterOwner) (chainPlan, error) {
	clusterSize, firstCid, totalCid := parser.ClusterHeapRange()
	if totalCid == 0 {
		return chainPlan{}, errors.New("cluster heap is empty")
	}
	lastCid := firstCid + totalCid

	if entry.FirstCluster < firstCid {
		return chainPlan{}, fmt.Errorf("entry %q has no cluster chain (first_cluster=%d)", entry.Path, entry.FirstCluster)
	}

	var plan chainPlan
	cid := entry.FirstCluster
	need := entry.DataLength // 0 表示长度未知（目录），一路走到链尾
	seen := make(map[uint32]struct{})

	// 链在 FAT 上停住时的现场，用于把「到底为什么短」写进 ShortChainError。
	var endCid, endFatNext uint32

	for {
		if cid < firstCid || cid >= lastCid {
			return chainPlan{}, fmt.Errorf("cluster %d out of heap range [%d,%d)", cid, firstCid, lastCid)
		}
		if _, dup := seen[cid]; dup {
			return chainPlan{}, fmt.Errorf("fat chain loop at cluster %d", cid)
		}
		seen[cid] = struct{}{}

		// 这一簇已被占住：链到此为止，前半段照收。占用者是谁要记下来，方便核对判定。
		if state[cid] == ClusterUsed {
			claim := owners[cid]
			if claim.Entry == "" {
				claim.Entry = "(unknown live entry)"
			}
			plan.stop = &OverwrittenError{
				Cluster:   cid,
				Owner:     claim.Entry,
				OwnerHead: claim.Head,
				OwnerLink: claim.Link,
				Path:      entry.Path,
			}
			return plan, nil
		}

		plan.chain = append(plan.chain, cid)

		if need > 0 {
			if need <= clusterSize {
				need = 0
				break // DataLength 已被这一簇覆盖，链到此为止
			}
			need -= clusterSize
		}

		if entry.NoFatChain {
			cid++ // 连续分配，簇号自增，不用查 FAT
			continue
		}
		_, next, err := parser.GetClusterFSInfo(cid)
		if err != nil {
			return chainPlan{}, fmt.Errorf("fat lookup cluster %d: %w", cid, err)
		}
		if next < firstCid || next >= lastCid {
			// 链到此为止。next 的原始值能区分两种成因，别混为一谈：
			// 0 = 这一簇已被释放（删文件时链上的 FAT 项被清空）；
			// 0x0FFFFFFF = 真正的链尾 EOC（装载时被 28 位掩码打成越界值）。
			endCid, endFatNext = cid, next
			break
		}
		cid = next
	}

	// FAT 说没有下一簇了，但 DataLength 还没喂饱。前段已经拿在手里、是完好的，
	// 后半段没有任何指针可达 —— 截断交出前段，但必须让调用方知道这份是残的。
	if need > 0 {
		plan.short = &ShortChainError{
			Shortfall:   need,
			Want:        entry.DataLength,
			Got:         entry.DataLength - need,
			LastCluster: endCid,
			LastFatNext: endFatNext,
			Path:        entry.Path,
		}
	}
	return plan, nil
}

// RestoreFile 把一个已删除文件条目指向的数据读出来写到 dst。
//
// state / owners 来自 BuildClusterState，可传 nil（跳过占用检查）。链走不到 DataLength 时
// 不全盘放弃，而是写出断点之前的部分并置 Truncated —— 断点成因看 Stop（撞上活文件）或
// Short（FAT 提前结束）。链本身无法解释（越界、成环、FAT 读失败）才是硬错误，一个字节都不写。
//
// dst 的父目录会按需建出来。直接传 /dev/xxx 这类设备节点也可以，但会从设备起始位置
// 覆盖原有内容 —— 务必确认那不是源盘。
func RestoreFile(parser fsinit.FileSystemParser, entry fsinit.FileEntryItem, state map[uint32]ClusterState, owners ClusterOwner, dst string) (RestoreResult, error) {
	plan, err := planChain(parser, entry, state, owners)
	if err != nil {
		return RestoreResult{}, err
	}
	return writePlan(parser, entry, plan, dst)
}

// writePlan 把 plan 里的簇读出来按顺序写进 dst，返回实际恢复量。
// chain 为空（链头就被占住）时一个字节都不写、也不建 dst：空文件会让人以为恢复成功了。
func writePlan(parser fsinit.FileSystemParser, entry fsinit.FileEntryItem, plan chainPlan, dst string) (RestoreResult, error) {
	res := RestoreResult{
		Clusters:  len(plan.chain),
		Truncated: plan.stop != nil || plan.short != nil,
		Stop:      plan.stop,
		Short:     plan.short,
	}
	if len(plan.chain) == 0 {
		return res, nil
	}

	// 父目录按需建出来；dst 是设备节点时 filepath.Dir 返回已存在的目录，MkdirAll 是空操作。
	if dir := filepath.Dir(dst); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return res, fmt.Errorf("create dir %s: %w", dir, err)
		}
	}

	// TODO(空目录): 目录层级是靠建文件的父目录顺带还原的，已删除的空目录不会被建出来。
	// 要还原完整目录树得另外遍历目录条目逐个 MkdirAll，前提是 FileEntryItem 暴露 IsDir，暂缓。

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return res, fmt.Errorf("open dst %s: %w", dst, err)
	}
	defer out.Close()

	left := entry.DataLength
	for _, cid := range plan.chain {
		chunk, err := parser.ReadCluster(cid)
		if err != nil {
			return res, fmt.Errorf("read cluster %d: %w", cid, err)
		}
		if left > 0 && uint64(len(chunk)) > left {
			chunk = chunk[:left] // 最后一簇通常只用到一部分
		}
		n, err := out.Write(chunk)
		if err != nil {
			return res, fmt.Errorf("write dst %s: %w", dst, err)
		}
		if n != len(chunk) {
			return res, fmt.Errorf("write dst %s: short write %d/%d", dst, n, len(chunk))
		}
		res.Bytes += uint64(len(chunk))
		if left > 0 {
			left -= uint64(len(chunk))
		}
	}
	return res, nil
}

// isSystemJunk 判断条目是不是文件系统自己的杂物：.Spotlight-V100 / .fseventsd / .Trashes /
// AppleDouble 的 ._xxx 之类。它们在目录表里同样算「已删除文件」，但恢复出来毫无意义。
// 判据取得很粗：路径任一段以 "." 开头就算 —— 代价是 .gitignore 这类文件也会被跳过。
func isSystemJunk(path string) bool {
	for part := range strings.SplitSeq(path, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// overlapAttrs 把「哪个簇被谁占了」摊成日志字段。err 不是 *OverwrittenError 时返回 nil。
func overlapAttrs(err error) []any {
	var ow *OverwrittenError
	// errors.As 对类型化的 nil 指针也会命中，但 ow 仍是 nil，直接用会 panic。
	if err == nil || !errors.As(err, &ow) || ow == nil {
		return nil
	}
	attrs := []any{
		slog.Int("overlap_cluster", int(ow.Cluster)),
		slog.String("overlap_owner", ow.Owner),
	}
	if ow.OwnerLink > 0 {
		attrs = append(attrs,
			slog.Int("overlap_owner_head", int(ow.OwnerHead)),
			slog.Int("overlap_owner_link", int(ow.OwnerLink)))
	}
	return attrs
}

// RestoreSummary 汇总一轮批量恢复的结果。Full / Partial 分两栏：完整的可以直接用，
// 残缺的那份得先人工确认。
type RestoreSummary struct {
	Candidates int    // 参与尝试的条目数（已删除、非目录、非系统杂物）
	Restored   int    // 真正写出数据的条数
	Full       int    // 其中整份恢复的
	Partial    int    // 其中只恢复了前半段的
	Failed     int    // 一个字节都没拿到的
	Bytes      uint64 // 写出数据的总字节数
}

// RestoreAllDeleted 把卷上所有「已删除、且不是目录」的条目逐个恢复出来。
//
// 已删除的目录跳过：目录条目不是文件，沿它的簇链读出来是一堆目录项。系统杂物同样跳过。
//
// 每个条目独立尝试、独立写盘，一个失败不影响下一个。三条出路的日志级别：
//   - 链能走通：整份恢复，Info done；
//   - 链走不到头（被占，或 FAT 提前结束）：恢复前半段，Warn partial —— 那一段是完好的，救回来就是净赚；
//   - 链头就被占（或链本身对不上）：一个字节都拿不到，Error failed。
//
// 输出落在 outRoot 下，路径沿用条目在卷上的位置（outRoot/a/b/c.txt）。只有「列出目录项」
// 失败才算错误；一个都没恢复出来是正常结果，由返回的汇总说明。
func RestoreAllDeleted(parser fsinit.FileSystemParser, state map[uint32]ClusterState, owners ClusterOwner, outRoot string, pre string, logger *slog.Logger) (RestoreSummary, error) {
	entries, err := parser.ListAllFileEntries()
	if err != nil {
		return RestoreSummary{}, fmt.Errorf("list file entries: %w", err)
	}

	// 先数一遍，否则恢复不出来时分不清是压根没有，还是全被新数据覆盖了。
	var deleted, deletedDirs, junk int
	for i := range entries {
		if !entries[i].IsDeleted {
			continue
		}
		deleted++
		switch {
		case entries[i].IsDir:
			deletedDirs++
		case isSystemJunk(entries[i].Path):
			junk++
		}
	}
	logger.Info("scan deleted entries", slog.String("pre", pre),
		slog.Int("total", len(entries)),
		slog.Int("deleted", deleted),
		slog.Int("deleted_dirs", deletedDirs),
		slog.Int("system_junk", junk),
		slog.Int("candidates", deleted-deletedDirs-junk))

	var sum RestoreSummary
	var lastErr error
	for i := range entries {
		e := entries[i]
		if !e.IsDeleted || e.IsDir {
			continue
		}
		if isSystemJunk(e.Path) {
			continue
		}
		sum.Candidates++

		// 属性名用 entry 而不是 file：SourceHandler 已经往每条记录里塞了一个 file
		// （源文件名），重名会让 JSON 里出现两个 file，把条目路径吃掉。
		logger.Info("restore candidate", slog.String("pre", pre),
			slog.Int("index", i),
			slog.String("entry", e.Path),
			slog.Uint64("size", e.DataLength),
			slog.Int("first_cluster", int(e.FirstCluster)))

		dst, err := outputPath(outRoot, e.Path)
		if err != nil {
			logRestoreFailed(logger, pre, e.Path, err)
			lastErr = err
			sum.Failed++
			continue
		}

		res, err := RestoreFile(parser, e, state, owners, dst)
		if err != nil {
			logRestoreFailed(logger, pre, e.Path, err)
			lastErr = err
			sum.Failed++
			continue
		}
		if res.Bytes == 0 {
			// 链头自己就被占住：一个字节都拿不回来，和失败没区别。
			logRestoreFailed(logger, pre, e.Path, res.Stop)
			lastErr = res.Stop
			sum.Failed++
			continue
		}

		sum.Restored++
		sum.Bytes += res.Bytes
		if res.Truncated {
			sum.Partial++
			logRestorePartial(logger, pre, e.Path, e.DataLength, dst, res)
			continue
		}
		sum.Full++
		logger.Info("restore file done", slog.String("pre", pre),
			slog.String("entry", e.Path),
			slog.Uint64("bytes", res.Bytes),
			slog.String("dst", dst))
	}

	switch {
	case sum.Candidates == 0:
		// 卷上压根没有可恢复的条目，正常结束。
		logger.Info("no deleted file candidate", slog.String("pre", pre))
	case sum.Restored == 0:
		// 候选全都没救回来：补一条总数，并带上最后一个占用者。有救回来的就不汇总了。
		attrs := []any{
			slog.String("pre", pre),
			slog.Int("candidates", sum.Candidates),
			slog.Any("last_err", lastErr),
		}
		attrs = append(attrs, overlapAttrs(lastErr)...)
		logger.Error("no deleted file restored", attrs...)
	}
	return sum, nil
}

// logRestorePartial 记一条「只恢复了前半段」的告警：残缺文件的大小是合法的，光看 dst 分不出来。
func logRestorePartial(logger *slog.Logger, pre, path string, want uint64, dst string, res RestoreResult) {
	attrs := []any{
		slog.String("pre", pre),
		slog.String("entry", path),
		slog.Uint64("bytes", res.Bytes),
		slog.Uint64("want_bytes", want),
		slog.Int("clusters", res.Clusters),
		slog.String("dst", dst),
	}
	attrs = append(attrs, overlapAttrs(res.Stop)...)
	if s := res.Short; s != nil {
		// 把「为什么短」摊开：last_fat_next 为 0 说明这一簇已被释放（旧数据多半还在盘上），
		// 0x0FFFFFFF 说明 FAT 本身就说这里是链尾 —— 两种情况的可恢复性完全不同。
		attrs = append(attrs,
			slog.Uint64("short_bytes", s.Shortfall),
			slog.Int("last_cluster", int(s.LastCluster)),
			slog.Uint64("last_fat_next", uint64(s.LastFatNext)),
			slog.Bool("cluster_released", s.LastFatNext == 0))
	}
	logger.Warn("restore file partial", attrs...)
}

// logRestoreFailed 记一条「一个字节都没救回来」的错误：链头被占、链本身对不上、
// 路径越狱都走这里。用 Error 而不是 Warn —— 这个文件是彻底没了，和「只恢复前半段」不是一回事。
func logRestoreFailed(logger *slog.Logger, pre, path string, err error) {
	attrs := []any{
		slog.String("pre", pre),
		slog.String("entry", path),
		slog.Any("err", err),
	}
	// 占用者从 err 里提出来单独成字段，省得用正则去抠字符串。
	attrs = append(attrs, overlapAttrs(err)...)
	logger.Error("restore file failed", attrs...)
}

// outputPath 把条目路径挂到输出根目录下，并挡住越狱。
// entry.Path 来自卷上的目录项，损坏或被伪造的卡完全可能给出 "../.." 这种名字 ——
// 要求拼出来的结果仍落在 outRoot 内。
func outputPath(outRoot, name string) (string, error) {
	if outRoot == "" {
		return "", errors.New("empty output root")
	}
	if name == "" {
		return "", errors.New("empty entry path")
	}
	dst := filepath.Join(outRoot, filepath.FromSlash(name))
	rel, err := filepath.Rel(outRoot, dst)
	if err != nil {
		return "", fmt.Errorf("resolve %q under %q: %w", name, outRoot, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("entry path %q escapes output root", name)
	}
	return dst, nil
}

// Engine 打开 path 指向的设备，识别文件系统，把上面的已删除文件恢复到 outRoot 下。
func Engine(path string, outRoot string, pre string, logger *slog.Logger) error {
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
	// DetectFileSystem 认不出来时返回的是 (nil, nil) 而不是错误，不挡住的话下一步会空指针 panic。
	if parser == nil {
		err := fmt.Errorf("unsupported file system on %s", path)
		logger.Error("detect fs err", slog.String("pre", pre),
			slog.Any("err", err), slog.String("path", path))
		return err
	}
	logger.Info("file system detected", slog.String("pre", pre),
		slog.String("fs", fmt.Sprintf("%T", parser)), slog.String("path", path))

	err = parser.Load(f)
	if err != nil {
		logger.Error("parser load err", slog.String("pre", pre),
			slog.Any("err", err), slog.String("path", path))
		return err
	}

	parser.DebugPrintMeta()

	// 记下未删除文件占用的簇，恢复时用它判断原簇是否已被覆盖。
	state, owners, err := BuildClusterState(parser, logger, pre)
	if err != nil {
		logger.Error("build cluster state err", slog.String("pre", pre),
			slog.Any("err", err), slog.String("path", path))
		return err
	}

	// 把所有可恢复的已删除文件捞出来，按原路径落到 outRoot 下。
	sum, err := RestoreAllDeleted(parser, state, owners, outRoot, pre, logger)
	if err != nil {
		logger.Error("restore deleted files err", slog.String("pre", pre),
			slog.Any("err", err), slog.String("path", path), slog.String("out_root", outRoot))
		return err
	}

	// full / partial 分开报：残缺的那几份得人工确认过才能用。
	logger.Info("restore deleted files done", slog.String("pre", pre),
		slog.Int("candidates", sum.Candidates),
		slog.Int("restored", sum.Restored),
		slog.Int("full", sum.Full),
		slog.Int("partial", sum.Partial),
		slog.Int("failed", sum.Failed),
		slog.Uint64("bytes", sum.Bytes),
		slog.String("out_root", outRoot))

	logger.Info("recovery end", slog.String("pre", pre), slog.String("path", path))
	return nil
}
