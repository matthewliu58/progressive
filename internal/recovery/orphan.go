package recovery

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"

	"progrescarve/internal/feature"
	"progrescarve/internal/feature/jpeg"
	"progrescarve/internal/feature/mov"
	"progrescarve/internal/feature/png"
	"progrescarve/internal/feature/wav"

	fsinit "progrescarve/internal/fs/finit"
)

// 这个文件只做一件事：把「扫描认出了文件头、但目录项里已经没有」的孤儿恢复出来。
//
// 为什么需要它：RestoreAllDeleted 完全由目录项驱动，而目录项槽位会被新文件复用
// 覆盖掉（往卡里写任何东西都可能触发）。条目没了，文件数据却可能还在空闲簇里 ——
// 扫描能从内容认出「这一簇是某个文件的头」，这是条目丢失之后唯一还剩的入口。
//
// 流程就四步：找到 header → 确认它是没人认领的真文件头 → 起假名落盘 → 尽量往后拼。
// 拼的时候跟普通删除条目走同一条路（pickLoose / pickTail 按簇号邻近拿），因为孤儿
// 和普通条目的处境一样：FAT 清了、没有地图，只能靠分配连续性。

// EnableOrphans 是孤儿恢复的总开关，默认关。
//
// 为什么默认关：它产出的是假名文件，且只有 JPEG / PNG 能靠结束标记定出准确长度，
// WAV 靠头里的 size 字段，MP4 / MOV 连结束标记都没有、长度只能靠解码器裁决。
// 跟目录项驱动的恢复混在一起会干扰对主流程结果的判断，要试就显式打开。
var EnableOrphans = true

// orphanDir 是孤儿文件专门的输出目录，挂在 outRoot 下面。
// 单独放一层是为了跟正常恢复出来的文件分开：孤儿没有真名，混在一起事后分不清。
const orphanDir = "_orphans"

// orphanMaxClusters 是孤儿最多顺着往后拼多少簇。
//
// 孤儿没有 DataLength，不知道文件多大，只能一路拼到结束标记或拼不动为止。给它
// 个上限兜底：256 簇 = 64MB，正常照片远到不了；超了说明结束标记没探到，再拼下去
// 只是把别人的簇往里塞。
const orphanMaxClusters = 256

// iendChunk 是 PNG 的 IEND 分块：[长度 0][类型 "IEND"][CRC]。
// 长度固定 0、CRC 固定 AE426082，12 个字节全是定值 —— 按它定位不会认错位置。
var iendChunk = []byte{0x00, 0x00, 0x00, 0x00, 'I', 'E', 'N', 'D', 0xAE, 0x42, 0x60, 0x82}

// orphanKinds 是认作「文件起点」的分类：只认文件头，不认后续碎片。
//
// payload（中间碎片）不能当起点 —— 它是某个文件的中间段，从它开始拼出来的东西
// 没有文件头，解码器一个都打不开。
//
// 只收「长度能定下来」的三种：JPEG 靠 EOI、PNG 靠 IEND、WAV 靠 RIFF 头里的 size。
//
// MP4 / MOV 不在此列，是实测打回来的：它们没有结束标记，孤儿又没有 DataLength，
// 于是 carve 只能一路吃到上限（256 簇 = 64MB），把大片无关的自由簇全吸进来；
// 而 MP4/MOV 的解码校验偏松（只验头部盒子），这么一坨照样放行，落地就是
// 「体积正常却打不开」的文件。与其靠校验兜底，不如根本不收。
var orphanKinds = []feature.Kind{
	feature.KindJPEGHeader,
	feature.KindPNGHeader,
	feature.KindWAVHeader,
}

// orphanExt 给出各格式落盘时用的扩展名。
func orphanExt(kind feature.Kind) string {
	switch kind {
	case feature.KindPNGHeader:
		return ".png"
	case feature.KindMP4Header:
		return ".mp4"
	case feature.KindMOVHeader:
		return ".mov"
	case feature.KindWAVHeader:
		return ".wav"
	default:
		return ".jpg"
	}
}

// isHeaderAtStart 判断这一簇的**开头**（偏移 0）就是该格式的文件头。
//
// 不能只信 Hit.IsFileHead()，原因是个实测踩到的坑：扫描里有些命中是整簇搜出来的
// （findFTYPBox 在簇中间找到 "ftyp"），它扫的是子切片 data[off:]，特征里的偏移是
// 相对子切片的 —— FTYPOffset 记成 0，于是 IsFileHead() 误报成「头在簇首」。
// 实际上那一簇开头还是上一个文件的尾巴。拿它当孤儿起点，写出来的文件 offset 0
// 就是垃圾，解码器必然拒：
//
//	mp4 box structure invalid at offset 0: first box is "\xea\xdaʺ", want "ftyp"
//
// 所以孤儿起点必须回读原始字节，用各格式自己的「偏移 0 判定」再过一遍。
func isHeaderAtStart(data []byte, kind feature.Kind) bool {
	switch kind {
	case feature.KindJPEGHeader:
		return jpeg.IsJPEGHeader(data)
	case feature.KindPNGHeader:
		return png.IsPNGHeader(data)
	case feature.KindWAVHeader:
		return wav.IsWAVHeader(data)
	case feature.KindMP4Header, feature.KindMOVHeader:
		// MP4 / MOV 共用 ISO 盒子结构，偏移 0 是个合法盒子头即可（ftyp 或 moov）。
		return mov.IsMOVHeader(data)
	default:
		return false
	}
}

// orphanName 拼出孤儿的落盘名字（含所在子目录）。
//
// 名字里带首簇号：事后要对着簇号回查这份是哪个孤儿拼出来的 —— 孤儿没有真名，
// 簇号是唯一能把输出文件和盘上位置对起来的东西。
//
// 名字在动手恢复**之前**就定下来：开始尝试的日志带上它，中途失败也能知道是
// 哪一份没成，不用靠猜。
func orphanName(cid uint32, kind feature.Kind) string {
	return fmt.Sprintf("%s/orphan_%d%s", orphanDir, cid, orphanExt(kind))
}

// RestoreOrphans 恢复扫描找到的孤儿文件头，返回（成功数，失败数）。
//
// claims 与 RestoreAllDeleted 共用同一张表：条目先跑、先认领，这里只在簇还没被
// 认领时才下手，保证孤儿不会把真实条目用过的簇抢走。
func RestoreOrphans(parser fsinit.FileSystemParser, state map[uint32]ClusterState,
	owners ClusterOwner, stats *ScanStats, claims map[uint32]string,
	outRoot string, pre string, logger *slog.Logger) (int, int) {

	if stats == nil || claims == nil {
		return 0, 0
	}
	clusterSize, _, _ := parser.ClusterHeapRange()
	if clusterSize == 0 {
		return 0, 0
	}

	// 孤儿的定义是「目录项已经没了，只剩内容里的文件头」。所以凡是还挂着目录项的
	// 头都不是孤儿 —— 按条目首簇集合过滤。
	//
	// 只用 claims 不够准：claims 是恢复过程中填的，条目若在第一簇就失败（比如被
	// 覆盖）根本不会进认领表，那一簇就会被误当成孤儿。按首簇集合过滤才贴合定义。
	entryFirsts := make(map[uint32]bool)
	if entries, err := parser.ListAllFileEntries(); err == nil {
		for i := range entries {
			entryFirsts[entries[i].FirstCluster] = true
		}
	}

	ok, failed := 0, 0
	for _, kind := range orphanKinds {
		for _, cid := range stats.Hits[kind] {
			if state[cid] == ClusterUsed {
				continue
			}
			// 还挂着目录项：不是孤儿，交给条目恢复那条路去处理。
			if entryFirsts[cid] {
				continue
			}
			// 已被真实条目（或别的孤儿）认领的跳过：先到先得，条目优先。
			if _, taken := claims[cid]; taken {
				continue
			}
			// 关键一步：文件头必须就在簇首。
			//
			// 只看 Hit.IsFileHead() 不够（原因见 isHeaderAtStart 的说明：整簇搜出来
			// 的命中扫的是子切片，会误报成头在簇首）。这里回读原始字节，用各格式
			// 自己的偏移 0 判定再过一遍 —— 文件真正的第一簇，头标记一定在偏移 0。
			hit, found := stats.HitAt(cid)
			if !found || !hit.IsFileHead() {
				continue
			}
			data, err := parser.ReadCluster(cid)
			if err != nil || !isHeaderAtStart(data, kind) {
				continue
			}

			// 名字先定下来：下面这条日志带上它，中途失败也知道是哪一份没成。
			name := orphanName(cid, kind)
			if logger != nil {
				logger.Info("restore orphan begin", slog.String("pre", pre),
					slog.Uint64("cluster", uint64(cid)),
					slog.String("kind", kind.String()),
					slog.String("entry", name))
			}

			if err := restoreOrphan(parser, state, owners, stats, claims,
				outRoot, pre, logger, cid, kind, name, clusterSize); err != nil {
				failed++
				if logger != nil {
					logger.Warn("restore orphan failed", slog.String("pre", pre),
						slog.Uint64("cluster", uint64(cid)),
						slog.String("kind", kind.String()),
						slog.Any("err", err))
				}
				continue
			}
			ok++
		}
	}

	if logger != nil {
		logger.Info("restore orphans done", slog.String("pre", pre),
			slog.Int("ok", ok), slog.Int("failed", failed),
			slog.String("dir", orphanDir))
	}
	return ok, failed
}

// restoreOrphan 恢复单个孤儿。cid 是扫描认出的文件头所在簇。
func restoreOrphan(parser fsinit.FileSystemParser, state map[uint32]ClusterState,
	owners ClusterOwner, stats *ScanStats, claims map[uint32]string,
	outRoot string, pre string, logger *slog.Logger,
	cid uint32, kind feature.Kind, name string, clusterSize uint64) error {

	// name 由调用方定好传进来（见 orphanName）：开始尝试的日志已经带上它了，
	// 这里沿用同一个名字，别再拼一次 —— 两处拼法不一致会让日志和输出对不上。
	entry := fsinit.FileEntryItem{
		Name:         fmt.Sprintf("orphan_%d%s", cid, orphanExt(kind)),
		Path:         name,
		FirstCluster: cid,
		DataLength:   orphanMaxClusters * clusterSize,
		IsDeleted:    true,
	}

	dst, err := outputPath(outRoot, name)
	if err != nil {
		return err
	}

	// 自己走 planChain + writePlan，而不是直接调 RestoreFile：要拿 plan 才能在
	// 末尾按结束标记截断 —— 孤儿没有真实 DataLength，不截就是一坨拼过头的垃圾。
	plan, err := planChain(parser, entry, state, owners, stats, claims, logger)
	if err != nil {
		return err
	}
	if len(plan.chain) == 0 {
		return fmt.Errorf("first cluster %d unusable", cid)
	}

	// 登记认领：chain 和猜出来的簇都算它的，后面的孤儿别再来抢。
	all := make([]uint32, 0, len(plan.chain)+len(plan.guessed))
	all = append(all, plan.chain...)
	all = append(all, plan.guessed...)
	for _, c := range all {
		claims[c] = name
	}

	res, err := writePlan(parser, entry, plan, dst, logger)
	if err != nil {
		unclaim(claims, all)
		return err
	}

	// 解码器不认：猜的那段是别人的数据，整份丢掉，别给用户一个体积正常却打不开的文件。
	if res.Verify != nil {
		_ = os.Remove(dst)
		unclaim(claims, all)
		return fmt.Errorf("rejected by decoder: %w", res.Verify.Err)
	}

	// 拼的时候不知道长度，写出来才按各自格式的收尾规则把尾巴切掉。
	//
	// 切不出结尾就必须整份丢掉：那说明拼到的长度是「拼到拼不动为止」—— 自由簇
	// 连成一片时能吃到上限（256 簇 = 64MB），里面绝大多数是无关数据。解码器还
	// 不一定拦得住（MP4/MOV 的校验就偏松，实测放行过一坨打不开的文件）。
	// 宁可不给，也别给一个体积正常却打不开的东西。
	off, ok := orphanEndOffset(parser, stats, all, plan.format, clusterSize)
	if !ok || off <= 0 || off > int64(res.Bytes) {
		_ = os.Remove(dst)
		unclaim(claims, all)
		return fmt.Errorf("orphan end not determinable: carved %d clusters / %d bytes",
			len(all), res.Bytes)
	}
	if err := os.Truncate(dst, off); err != nil {
		return fmt.Errorf("truncate orphan to %d: %w", off, err)
	}
	bytes := uint64(off)

	if logger != nil {
		logger.Info("restore orphan", slog.String("pre", pre),
			slog.String("entry", name),
			slog.Uint64("first_cluster", uint64(cid)),
			slog.Uint64("bytes", bytes),
			slog.Int("clusters", len(all)),
			slog.Int("carved", len(plan.guessed)),
			slog.String("dst", dst))
	}
	return nil
}

// orphanEndOffset 给出孤儿该在总长度的第几字节截断；拿不到准确长度返回 false。
//
// 按簇顺序找**第一个**结束标记，不是最后一个：EOI / IEND 之后的簇都是拼过头
// 多拿的，拿最后一个会把垃圾的位置当成文件尾。
//
// MP4 / MOV 没有结束标记，这里返回 false —— 它们不截断，长度交给解码器裁决
// （verify 过了就留着，过不了整个删掉）。
func orphanEndOffset(parser fsinit.FileSystemParser, stats *ScanStats, all []uint32,
	format feature.Kind, clusterSize uint64) (int64, bool) {

	total := int64(len(all)) * int64(clusterSize)

	switch format {
	case feature.KindWAVHeader, feature.KindWAVPayload:
		// WAV 的长度写在 RIFF 头里：size 字段 = 文件总长 - 8。
		// 比结束标记还省事，前提是写文件那次是正常收尾的 —— 录音中断时它可能是 0
		// 或残留值，那种情况返回 false，交给 verify。
		if len(all) == 0 {
			return 0, false
		}
		data, err := parser.ReadCluster(all[0])
		if err != nil || len(data) < 12 {
			return 0, false
		}
		size := uint64(binary.LittleEndian.Uint32(data[4:8])) + 8
		if size == 8 || size > uint64(total) {
			return 0, false // 长度不合理，别照着截
		}
		return int64(size), true
	}

	for i, cid := range all {
		h, ok := stats.HitAt(cid)
		if !ok {
			continue
		}
		base := int64(i) * int64(clusterSize)

		switch format {
		case feature.KindJPEGHeader, feature.KindJPEGPayload:
			if h.JPEG != nil && h.JPEG.EOIOffset >= 0 {
				// EOI 是 FF D9 两个字节，截断位置在它之后。
				return base + int64(h.JPEG.EOIOffset) + 2, true
			}

		case feature.KindPNGHeader, feature.KindPNGPayload:
			// PNG 的 Feature 只记了「有没有 IEND」，没记偏移，所以回读这一簇按
			// IEND 分块的定值字节定位。
			if h.PNG != nil && h.PNG.HasIEND {
				data, err := parser.ReadCluster(cid)
				if err != nil {
					return 0, false
				}
				if pos := bytes.Index(data, iendChunk); pos >= 0 {
					return base + int64(pos) + int64(len(iendChunk)), true
				}
			}
		}
	}
	return 0, false
}

// unclaim 把 clusters 从认领表里撤掉：这份没落地成文件，簇要让出来给别人。
func unclaim(claims map[uint32]string, clusters []uint32) {
	for _, c := range clusters {
		delete(claims, c)
	}
}
