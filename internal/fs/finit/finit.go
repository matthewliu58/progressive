package finit

import "os"

// FileEntryItem 是目录表里的一条记录，不管删除与否都在里面。
type FileEntryItem struct {
	Name         string // 文件名（不含路径）
	Path         string // 卷上的完整路径，用 / 分隔
	FirstCluster uint32 // 文件数据的第一簇；簇号小于合法范围时表示没有簇链
	DataLength   uint64 // 目录项声明的文件大小（字节）；0 通常是目录
	ValidLength  uint64 // 实际写入的字节数，可能小于 DataLength
	IsDeleted    bool   // 是否已删除
	IsDir        bool   // 是否目录：目录的簇链读出来是一堆目录项，不是文件内容
	NoFatChain   bool   // 连续分配标志：置位表示簇号从 FirstCluster 起自增，不用查 FAT
}

// FileSystemParser 是统一的文件系统解析器接口。
// 恢复流程只认这个接口，不关心底下是 exFAT 还是别的什么卷。
type FileSystemParser interface {
	// Load 把已打开的设备读一遍，把元数据（引导扇区、FAT、位图、目录树）载入内存。
	Load(f *os.File) error

	// GetClusterFSInfo 查一簇在文件系统里的状态：
	// alloc 是分配位图给的答案，fatNext 是 FAT 表里记的下一簇。
	GetClusterFSInfo(cid uint32) (alloc bool, fatNext uint32, err error)

	// ListAllFileEntries 列出目录树里的所有条目，已删除的也在里面。
	ListAllFileEntries() ([]FileEntryItem, error)

	// ClusterHeapRange 给出簇堆的范围：簇大小、第一个簇号、簇总数。
	// 合法簇号区间是 [firstCluster, firstCluster+totalCluster)。
	ClusterHeapRange() (clusterSize uint64, firstCluster uint32, totalCluster uint32)
	// SystemClusters 返回簇堆里承载文件系统公共信息（分配位图、根目录、
	// up-case 表……）的簇号，它们不承载任何可恢复的文件内容。
	SystemClusters() []uint32

	// ReadCluster 读出一簇的原始内容。走的是 ReadAt：位置无关、并发安全。
	ReadCluster(cid uint32) ([]byte, error)

	// DebugPrintMeta 把解析出来的元数据打进日志，排查卷本身的问题时用。
	DebugPrintMeta()
}
