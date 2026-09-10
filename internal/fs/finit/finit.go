package finit

import "os"

type FileEntryItem struct {
	Name         string
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
	ReadCluster(cid uint32) ([]byte, error)
	DebugPrintMeta()
}
