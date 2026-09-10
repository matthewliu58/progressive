package recovery

import (
	"log/slog"
	"os"
	"progrescarve/internal/fs"
)

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

	//// 拿到簇范围（接口方法）
	//clusterSize, firstCid, totalCid := parser.ClusterHeapRange()
	//logPrintf("\nClusterHeap: clusterSize=%d, firstCid=%d, totalCid=%d\n", clusterSize, firstCid, totalCid)
	//
	//// 列出所有目录条目
	//entries, err := parser.ListAllFileEntries()
	//if err == nil {
	//	logPrintf("\n=== File Entries(count=%d) ===\n", len(entries))
	//	for _, e := range entries {
	//		logPrintf("name=%q deleted=%v firstCid=%d len=%d\n", e.Name, e.IsDeleted, e.FirstCluster, e.DataLength)
	//	}
	//}

	// 调用通用carving，传入接口，不感知exfat
	//_ = ScanFreeClusters(parser, logPrintf)

	logger.Info("recovery end", slog.String("pre", pre), slog.String("path", path))
	return nil
}
