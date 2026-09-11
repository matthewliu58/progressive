package finit

import "os"

type FileEntryItem struct {
	Name         string
	Path         string
	FirstCluster uint32
	DataLength   uint64
	ValidLength  uint64
	IsDeleted    bool
	NoFatChain   bool
}

// FileSystemParser 统一文件系统解析器接口
type FileSystemParser interface {
	Load(f *os.File) error
	GetClusterFSInfo(cid uint32) (alloc bool, fatNext uint32, err error)
	ListAllFileEntries() ([]FileEntryItem, error)
	ClusterHeapRange() (clusterSize uint64, firstCluster uint32, totalCluster uint32)
	// SystemClusters 返回簇堆里承载文件系统公共信息（分配位图、根目录、
	// up-case 表……）的簇号，它们不承载任何可恢复的文件内容。
	SystemClusters() []uint32
	ReadCluster(cid uint32) ([]byte, error)
	DebugPrintMeta()
}
