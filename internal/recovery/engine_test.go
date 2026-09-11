package recovery

import (
	"os"
	"testing"

	fsinit "progrescarve/internal/fs/finit"
)

// fakeParser 用内存数据实现 FileSystemParser，只为验证 BuildClusterState
// 的标记规则，不碰真实镜像。
type fakeParser struct {
	clusterSize uint64
	firstCid    uint32
	totalCid    uint32
	system      []uint32
	entries     []fsinit.FileEntryItem
	alloc       map[uint32]bool
	fat         map[uint32]uint32
}

func (f *fakeParser) Load(*os.File) error { return nil }

func (f *fakeParser) GetClusterFSInfo(cid uint32) (bool, uint32, error) {
	return f.alloc[cid], f.fat[cid], nil
}

func (f *fakeParser) ListAllFileEntries() ([]fsinit.FileEntryItem, error) {
	return f.entries, nil
}

func (f *fakeParser) ClusterHeapRange() (uint64, uint32, uint32) {
	return f.clusterSize, f.firstCid, f.totalCid
}

func (f *fakeParser) ReadCluster(uint32) ([]byte, error) { return nil, nil }

func (f *fakeParser) DebugPrintMeta() {}

func (f *fakeParser) SystemClusters() []uint32 { return f.system }

func TestBuildClusterState(t *testing.T) {
	p := &fakeParser{
		clusterSize: 512,
		firstCid:    2,
		totalCid:    10, // 簇号区间 [2, 12)
		system:      []uint32{2},
		fat:         map[uint32]uint32{3: 4, 9: 9},
		alloc: map[uint32]bool{
			2: true, 3: true, 4: true, 5: true, 8: true, 9: true,
		},
		entries: []fsinit.FileEntryItem{
			{Name: "A", FirstCluster: 3, DataLength: 1024},                  // FAT 链 3→4→尾
			{Name: "B", FirstCluster: 5, DataLength: 512, NoFatChain: true}, // 连续 1 簇
			{Name: "C", FirstCluster: 9},                                    // FAT 自环，应被截断
			{Name: "D", FirstCluster: 8, DataLength: 512, IsDeleted: true},  // 已删除，不参与标记
		},
	}

	state, err := BuildClusterState(p, nil, "test")
	if err != nil {
		t.Fatalf("BuildClusterState: %v", err)
	}

	want := map[uint32]ClusterState{
		2:  ClusterUsed,    // 公共信息簇
		3:  ClusterUsed,    // FAT 链
		4:  ClusterUsed,    // FAT 链
		5:  ClusterUsed,    // NoFatChain 连续簇
		9:  ClusterUsed,    // 自环，链被截断但簇仍算占用
		8:  ClusterUnknown, // 位图说占用，但只有已删除条目指向它 —— 归属不明
		6:  ClusterFree,
		7:  ClusterFree,
		10: ClusterFree,
		11: ClusterFree,
	}

	if len(state) != len(want) {
		t.Fatalf("state 覆盖 %d 个簇，期望 %d 个", len(state), len(want))
	}
	for cid, w := range want {
		if got := state[cid]; got != w {
			t.Errorf("簇 %d = %d, want %d", cid, got, w)
		}
	}
}
