package recovery

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	clusters    map[uint32][]byte
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

func (f *fakeParser) ReadCluster(cid uint32) ([]byte, error) {
	if buf, ok := f.clusters[cid]; ok {
		return buf, nil
	}
	return nil, fmt.Errorf("cluster %d not found", cid)
}

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
			{Name: "A", Path: "A", FirstCluster: 3, DataLength: 1024},                  // FAT 链 3→4→尾
			{Name: "B", Path: "B", FirstCluster: 5, DataLength: 512, NoFatChain: true}, // 连续 1 簇
			{Name: "C", Path: "C", FirstCluster: 9},                                    // FAT 自环，应被截断
			{Name: "D", Path: "D", FirstCluster: 8, DataLength: 512, IsDeleted: true},  // 已删除，不参与标记
		},
	}

	state, owners, err := BuildClusterState(p, nil, "test")
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

	// 占用者要能说清是谁、以及链上第几环 —— 只给路径，对 first_cluster 会以为报错了。
	wantOwner := map[uint32]ClusterClaim{
		2: {Entry: systemOwner},           // 系统簇没有链
		3: {Entry: "A", Head: 3, Link: 1}, // FAT 链 3→4 的第一环
		4: {Entry: "A", Head: 3, Link: 2}, // 第二环：链头是 3，不是 4
		5: {Entry: "B", Head: 5, Link: 1}, // NoFatChain 连续簇
		9: {Entry: "C", Head: 9, Link: 1}, // 自环，标记第一环后截断
	}
	for cid, w := range wantOwner {
		if got := owners[cid]; got != w {
			t.Errorf("簇 %d 的占用者 = %+v, want %+v", cid, got, w)
		}
	}
	// 归属不明，不能记成「被某个活文件占着」，否则错误信息会指向一个无辜的文件。
	if got, ok := owners[8]; ok {
		t.Errorf("簇 8 不该有占用者, got %+v", got)
	}
}

func TestRestoreFile(t *testing.T) {
	fill := func(b byte) []byte {
		buf := make([]byte, 512)
		for i := range buf {
			buf[i] = b
		}
		return buf
	}

	newParser := func() *fakeParser {
		return &fakeParser{
			clusterSize: 512,
			firstCid:    2,
			totalCid:    10,
			fat:         map[uint32]uint32{3: 4},
			clusters: map[uint32][]byte{
				3: fill(0xAA),
				4: fill(0xBB),
			},
		}
	}

	// DataLength=700：首簇写满 512 字节，尾簇只写 188 字节。
	entry := fsinit.FileEntryItem{
		Name: "D.TXT", Path: "D.TXT",
		FirstCluster: 3, DataLength: 700, IsDeleted: true,
	}

	// 链上两簇都是空闲 —— 没有被覆盖，可以整份恢复。
	freeState := map[uint32]ClusterState{3: ClusterFree, 4: ClusterFree}

	dst := filepath.Join(t.TempDir(), "D.TXT")
	res, err := RestoreFile(newParser(), entry, freeState, nil, dst)
	if err != nil {
		t.Fatalf("RestoreFile: %v", err)
	}
	if res.Truncated || res.Bytes != 700 || res.Clusters != 2 {
		t.Errorf("整份恢复的结果 = %+v, want {Bytes:700 Clusters:2 Truncated:false}", res)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if len(got) != 700 {
		t.Fatalf("恢复出 %d 字节，期望 700", len(got))
	}
	if got[0] != 0xAA || got[511] != 0xAA {
		t.Errorf("首簇内容不对")
	}
	if got[512] != 0xBB || got[699] != 0xBB {
		t.Errorf("尾簇内容不对，且应被截到剩余长度")
	}

	// 恢复到还不存在的多级目录：层级要按需建出来，内容照写。
	nested := filepath.Join(t.TempDir(), "SUB", "DEEP", "D.TXT")
	if _, err := RestoreFile(newParser(), entry, freeState, nil, nested); err != nil {
		t.Fatalf("RestoreFile 到多级目录: %v", err)
	}
	if got, err := os.ReadFile(nested); err != nil {
		t.Fatalf("read nested restored file: %v", err)
	} else if len(got) != 700 {
		t.Errorf("多级目录下恢复出 %d 字节，期望 700", len(got))
	}

	// DataLength 要 3 簇但链只有 3→4，且 FAT[4] 没有下一簇：链提前断了，差 512 字节。
	// 这不是硬错误 —— 前段（2 簇 / 1024 字节）是完好的，必须照写，但要标成残缺并说清为什么短。
	short := entry
	short.DataLength = 1536
	shortDst := filepath.Join(t.TempDir(), "SHORT.TXT")
	sres, serr := RestoreFile(newParser(), short, freeState, nil, shortDst)
	if serr != nil {
		t.Fatalf("链提前结束时应恢复前半段，而不是整份失败: %v", serr)
	}
	if !sres.Truncated || sres.Bytes != 1024 || sres.Clusters != 2 {
		t.Errorf("链提前结束的结果 = %+v, want {Bytes:1024 Clusters:2 Truncated:true}", sres)
	}
	if sres.Short == nil {
		t.Fatal("Truncated 时必须说清链停在哪一簇、FAT 项是什么值")
	}
	// 结构化字段要能被调用方直接取出来，不能只活在字符串里（Engine 就是这么用的）。
	var sc *ShortChainError
	if !errors.As(sres.Short, &sc) || sc != sres.Short {
		t.Errorf("Short 应能经 errors.As 取出, got %v", sres.Short)
	}
	// 测试镜像里没给 FAT[4] 赋过值，取值 0，代表这一簇已被释放。
	if sc.Shortfall != 512 || sc.Want != 1536 || sc.Got != 1024 ||
		sc.LastCluster != 4 || sc.LastFatNext != 0 || sc.Path != short.Path {
		t.Errorf("ShortChainError = %+v, want {Shortfall:512 Want:1536 Got:1024 LastCluster:4 LastFatNext:0 Path:%q}",
			sc, short.Path)
	}
	if got, err := os.ReadFile(shortDst); err != nil {
		t.Fatalf("read short file: %v", err)
	} else if len(got) != 1024 {
		t.Errorf("残档应写出断点之前的 1024 字节, got %d", len(got))
	}

	// 同样短一截，但 FAT[4] 是 EOC 而不是 0：成因不同，得能区分开（前者可期待旧数据还在，
	// 后者说明目录项的 DataLength 与链本身就不自洽）。
	eocParser := newParser()
	eocParser.fat = map[uint32]uint32{3: 4, 4: 0x0FFFFFFF}
	eocDst := filepath.Join(t.TempDir(), "EOC.TXT")
	eres, eerr := RestoreFile(eocParser, short, freeState, nil, eocDst)
	if eerr != nil {
		t.Fatalf("链尾早于 DataLength 时也该恢复前半段: %v", eerr)
	}
	if eres.Short == nil || eres.Short.LastFatNext != 0x0FFFFFFF || eres.Short.Shortfall != 512 {
		t.Errorf("EOC 成因的 Short = %+v, want {LastFatNext:0x0FFFFFFF Shortfall:512}", eres.Short)
	}
	if eres.Short != nil && strings.Contains(eres.Short.Error(), "cluster released") {
		t.Errorf("EOC 不该被说成「簇已释放」: %v", eres.Short)
	}

	// 没有簇链：这才是真元数据异常，一个字节都不该写。
	noChain := fsinit.FileEntryItem{Name: "E.TXT", Path: "E.TXT", FirstCluster: 0}
	if _, err := RestoreFile(newParser(), noChain, nil, nil, dst); err == nil {
		t.Error("FirstCluster=0 时应返回失败")
	}

	// FAT 成环：同样无法解释，不能拿一段充数。
	loop := entry
	loop.FirstCluster = 9
	loopParser := newParser()
	loopParser.fat = map[uint32]uint32{9: 9}
	loopParser.clusters = map[uint32][]byte{9: fill(0xCC)}
	if _, err := RestoreFile(loopParser, loop, freeState, nil, dst); err == nil {
		t.Error("FAT 成环时应返回失败")
	}

	// 元数据异常时连目标文件都不该创建，多级父目录也不该被建出来。
	anomalous := filepath.Join(t.TempDir(), "SUB", "NEVER.TXT")
	if _, err := RestoreFile(newParser(), noChain, freeState, nil, anomalous); err == nil {
		t.Error("FirstCluster=0 时应返回失败")
	}
	if _, err := os.Stat(anomalous); !os.IsNotExist(err) {
		t.Errorf("元数据异常时不应创建目标文件, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Dir(anomalous)); !os.IsNotExist(err) {
		t.Errorf("元数据异常时不应创建父目录, stat err=%v", err)
	}

	// 链上有一簇被未删除的数据占住：只恢复前半段，而且这不算错误。
	overwritten := map[uint32]ClusterState{3: ClusterFree, 4: ClusterUsed}
	owner := ClusterOwner{4: {Entry: "LIVE.TXT", Head: 3, Link: 2}}
	sentinel := []byte("keep me intact")
	if err := os.WriteFile(dst, sentinel, 0o600); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	pres, perr := RestoreFile(newParser(), entry, overwritten, owner, dst)
	if perr != nil {
		t.Fatalf("撞上被占住的簇时应部分恢复，而不是失败: %v", perr)
	}
	if !pres.Truncated || pres.Bytes != 512 || pres.Clusters != 1 {
		t.Errorf("部分恢复的结果 = %+v, want {Bytes:512 Clusters:1 Truncated:true}", pres)
	}
	if pres.Stop == nil {
		t.Fatal("Truncated 时必须说清断在哪、被谁占了")
	}
	// 结构化字段要能被调用方直接取出来，不能只活在字符串里（Engine 就是这么用的）。
	var ow *OverwrittenError
	if !errors.As(pres.Stop, &ow) || ow != pres.Stop {
		t.Errorf("Stop 应能经 errors.As 取出, got %v", pres.Stop)
	}
	if ow.Cluster != 4 || ow.Owner != "LIVE.TXT" ||
		ow.OwnerHead != 3 || ow.OwnerLink != 2 || ow.Path != entry.Path {
		t.Errorf("OverwrittenError = %+v, want {Cluster:4 Owner:LIVE.TXT OwnerHead:3 OwnerLink:2 Path:%q}",
			ow, entry.Path)
	}
	// 这个字符串会原样进 err 字段：要点名占用者，还要给出链头 + 链上第几环。
	if !strings.Contains(ow.Error(), "LIVE.TXT") || !strings.Contains(ow.Error(), "chain head 3, link 2") {
		t.Errorf("Stop 的文本应点名占用者与链上位置, got %v", ow)
	}
	// 断点之前那一簇是完好的，必须原样写出来。
	if b, err := os.ReadFile(dst); err != nil {
		t.Fatalf("read dst: %v", err)
	} else if len(b) != 512 || b[0] != 0xAA || b[511] != 0xAA {
		t.Errorf("部分恢复应写回断点之前那一簇, got %d 字节", len(b))
	}

	// 链头就被占住：一个字节都拿不回来，连 dst 都不该建 —— 空文件会让人以为恢复成功了。
	headGone := filepath.Join(t.TempDir(), "HEAD.GONE")
	hres, herr := RestoreFile(newParser(), entry, map[uint32]ClusterState{3: ClusterUsed},
		ClusterOwner{3: {Entry: "LIVE.TXT"}}, headGone)
	if herr != nil {
		t.Fatalf("链头被占住不该是硬错误: %v", herr)
	}
	if !hres.Truncated || hres.Bytes != 0 || hres.Clusters != 0 {
		t.Errorf("链头被占住的结果 = %+v, want {Bytes:0 Clusters:0 Truncated:true}", hres)
	}
	if _, err := os.Stat(headGone); !os.IsNotExist(err) {
		t.Errorf("一个字节都没恢复时不应创建目标文件, stat err=%v", err)
	}
}

func TestRestoreAllDeleted(t *testing.T) {
	blk := func(b byte) []byte {
		buf := make([]byte, 512)
		for i := range buf {
			buf[i] = b
		}
		return buf
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	pre := "test"

	// 未删除的条目、已删除的目录、macOS 隐藏杂物都要跳过，最终只恢复 DEAD.TXT。
	outRoot := filepath.Join(t.TempDir(), "out")
	p := &fakeParser{
		clusterSize: 512,
		firstCid:    2,
		totalCid:    10,
		fat:         map[uint32]uint32{5: 6},
		clusters:    map[uint32][]byte{5: blk(0xAA), 6: blk(0xBB), 9: blk(0xDD)},
		entries: []fsinit.FileEntryItem{
			{Name: "LIVE.TXT", Path: "LIVE.TXT", FirstCluster: 3, DataLength: 512},
			{Name: "DIR", Path: "DIR", FirstCluster: 7, IsDeleted: true, IsDir: true},
			// 簇是空闲的，过滤一旦失效它就会多写一份，计数立刻对不上。
			{Name: "tmp.0", Path: ".Spotlight-V100/Store-V2/tmp.0", FirstCluster: 9, DataLength: 512, IsDeleted: true},
			{Name: "DEAD.TXT", Path: "DEAD.TXT", FirstCluster: 5, DataLength: 700, IsDeleted: true},
		},
	}
	sum, err := RestoreAllDeleted(p, map[uint32]ClusterState{5: ClusterFree, 6: ClusterFree, 9: ClusterFree}, nil, outRoot, pre, quiet)
	if err != nil {
		t.Fatalf("RestoreAllDeleted: %v", err)
	}
	if sum.Candidates != 1 || sum.Restored != 1 || sum.Full != 1 || sum.Partial != 0 || sum.Failed != 0 {
		t.Errorf("汇总 = %+v, want {Candidates:1 Restored:1 Full:1 Partial:0 Failed:0}", sum)
	}
	if sum.Bytes != 700 {
		t.Errorf("写出字节 = %d, want 700", sum.Bytes)
	}
	// 输出路径沿用条目在卷上的位置。
	if b, err := os.ReadFile(filepath.Join(outRoot, "DEAD.TXT")); err != nil {
		t.Fatalf("read restored file: %v", err)
	} else if len(b) != 700 {
		t.Errorf("恢复出 %d 字节，期望 700", len(b))
	}
	if _, err := os.Stat(filepath.Join(outRoot, ".Spotlight-V100", "Store-V2", "tmp.0")); !os.IsNotExist(err) {
		t.Errorf("系统杂物不应被恢复, stat err=%v", err)
	}

	// 每个条目独立尝试：链头被占的一个字节都拿不到，链完好的照常恢复。
	outRoot2 := filepath.Join(t.TempDir(), "out")
	p2 := &fakeParser{
		clusterSize: 512,
		firstCid:    2,
		totalCid:    10,
		clusters:    map[uint32][]byte{8: blk(0xCC)},
		entries: []fsinit.FileEntryItem{
			{Name: "GONE.TXT", Path: "GONE.TXT", FirstCluster: 5, DataLength: 512, IsDeleted: true},
			{Name: "OK.TXT", Path: "OK.TXT", FirstCluster: 8, DataLength: 512, IsDeleted: true},
		},
	}
	sum2, err := RestoreAllDeleted(p2, map[uint32]ClusterState{5: ClusterUsed, 8: ClusterFree}, nil, outRoot2, pre, quiet)
	if err != nil {
		t.Fatalf("RestoreAllDeleted: %v", err)
	}
	if sum2.Candidates != 2 || sum2.Restored != 1 || sum2.Failed != 1 {
		t.Errorf("汇总 = %+v, want {Candidates:2 Restored:1 Failed:1}", sum2)
	}
	if _, err := os.ReadFile(filepath.Join(outRoot2, "OK.TXT")); err != nil {
		t.Errorf("OK.TXT 应恢复出来: %v", err)
	}
	// 一个字节都没拿到时不该建文件 —— 建个空文件只会让人以为恢复成功了。
	if _, err := os.Stat(filepath.Join(outRoot2, "GONE.TXT")); !os.IsNotExist(err) {
		t.Errorf("一个字节都没恢复时不应创建目标文件, stat err=%v", err)
	}

	// 一个可恢复的都没有：不算错误，由汇总说明。
	outRoot3 := filepath.Join(t.TempDir(), "out")
	p3 := &fakeParser{
		clusterSize: 512,
		firstCid:    2,
		totalCid:    10,
		entries: []fsinit.FileEntryItem{
			{Name: "LIVE.TXT", Path: "LIVE.TXT", FirstCluster: 3, DataLength: 512},
			{Name: "DIR", Path: "DIR", FirstCluster: 7, IsDeleted: true, IsDir: true},
		},
	}
	sum3, err := RestoreAllDeleted(p3, nil, nil, outRoot3, pre, quiet)
	if err != nil {
		t.Fatalf("没有可恢复的条目不该是错误: %v", err)
	}
	if sum3 != (RestoreSummary{}) {
		t.Errorf("汇总 = %+v, want 零值", sum3)
	}

	// 不挑、都恢复：一份只能取前半段，一份完整 —— 两份都要各自落盘。
	outRoot4 := filepath.Join(t.TempDir(), "out")
	p4 := &fakeParser{
		clusterSize: 512,
		firstCid:    2,
		totalCid:    10,
		fat:         map[uint32]uint32{5: 6},
		clusters:    map[uint32][]byte{5: blk(0xAA), 6: blk(0xBB), 8: blk(0xCC)},
		entries: []fsinit.FileEntryItem{
			{Name: "PART.TXT", Path: "PART.TXT", FirstCluster: 5, DataLength: 1024, IsDeleted: true},
			{Name: "FULL.TXT", Path: "FULL.TXT", FirstCluster: 8, DataLength: 512, IsDeleted: true},
		},
	}
	sum4, err := RestoreAllDeleted(p4, map[uint32]ClusterState{5: ClusterFree, 6: ClusterUsed, 8: ClusterFree}, nil, outRoot4, pre, quiet)
	if err != nil {
		t.Fatalf("RestoreAllDeleted: %v", err)
	}
	if sum4.Restored != 2 || sum4.Full != 1 || sum4.Partial != 1 || sum4.Failed != 0 || sum4.Bytes != 1024 {
		t.Errorf("汇总 = %+v, want {Restored:2 Full:1 Partial:1 Failed:0 Bytes:1024}", sum4)
	}
	if b, err := os.ReadFile(filepath.Join(outRoot4, "PART.TXT")); err != nil {
		t.Fatalf("read PART.TXT: %v", err)
	} else if len(b) != 512 || b[0] != 0xAA {
		t.Errorf("残缺的那份只写回断点之前那一簇, got %d 字节", len(b))
	}
	if b, err := os.ReadFile(filepath.Join(outRoot4, "FULL.TXT")); err != nil {
		t.Fatalf("read FULL.TXT: %v", err)
	} else if len(b) != 512 || b[0] != 0xCC {
		t.Errorf("FULL.TXT 内容不对, got %d 字节", len(b))
	}

	// 原卷里的目录层级要一并还原。
	outRoot5 := filepath.Join(t.TempDir(), "out")
	p5 := &fakeParser{
		clusterSize: 512,
		firstCid:    2,
		totalCid:    10,
		clusters:    map[uint32][]byte{5: blk(0xEE)},
		entries: []fsinit.FileEntryItem{
			{Name: "D.TXT", Path: "SUB/DEEP/D.TXT", FirstCluster: 5, DataLength: 512, IsDeleted: true},
		},
	}
	if _, err := RestoreAllDeleted(p5, map[uint32]ClusterState{5: ClusterFree}, nil, outRoot5, pre, quiet); err != nil {
		t.Fatalf("RestoreAllDeleted: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(outRoot5, "SUB", "DEEP", "D.TXT")); err != nil {
		t.Errorf("应还原原卷目录层级: %v", err)
	}

	// 目录项可能被伪造出 ".."：isSystemJunk 先滤掉，outputPath 再兜一道。
	base := t.TempDir()
	outRoot6 := filepath.Join(base, "root")
	p6 := &fakeParser{
		clusterSize: 512,
		firstCid:    2,
		totalCid:    10,
		clusters:    map[uint32][]byte{5: blk(0x99)},
		entries: []fsinit.FileEntryItem{
			{Name: "ESCAPE.TXT", Path: "../ESCAPE.TXT", FirstCluster: 5, DataLength: 512, IsDeleted: true},
		},
	}
	if _, err := RestoreAllDeleted(p6, map[uint32]ClusterState{5: ClusterFree}, nil, outRoot6, pre, quiet); err != nil {
		t.Fatalf("RestoreAllDeleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "ESCAPE.TXT")); !os.IsNotExist(err) {
		t.Errorf("越狱路径不该写出文件, stat err=%v", err)
	}
	// outputPath 是最后一道：即使调用方漏了过滤，也不能拼出根目录之外的路径。
	for _, name := range []string{"../ESCAPE.TXT", "a/../../ESCAPE.TXT"} {
		if _, err := outputPath(outRoot6, name); err == nil {
			t.Errorf("outputPath(%q) 应报错", name)
		}
	}
	if got, err := outputPath(outRoot6, "SUB/D.TXT"); err != nil || got != filepath.Join(outRoot6, "SUB", "D.TXT") {
		t.Errorf("outputPath(SUB/D.TXT) = %q, %v", got, err)
	}
	// 绝对路径要被收进输出根，而不是按绝对路径写出去。
	if got, err := outputPath(outRoot6, "/etc/passwd"); err != nil || got != filepath.Join(outRoot6, "etc", "passwd") {
		t.Errorf("outputPath(/etc/passwd) = %q, %v", got, err)
	}
}

// TestOverlapOwnerLogged 确认「被哪个未删除文件占住了」真的落成了日志字段。
func TestOverlapOwnerLogged(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	p := &fakeParser{
		clusterSize: 512,
		firstCid:    2,
		totalCid:    10,
		entries: []fsinit.FileEntryItem{
			{Name: "DEAD.TXT", Path: "DEAD.TXT", FirstCluster: 4, DataLength: 512, IsDeleted: true},
		},
	}
	outRoot := t.TempDir()

	// 簇 4 被 LIVE.TXT 占着（链头 3、链上第 2 环），日志必须点名它并说清它不是链头。
	sum, err := RestoreAllDeleted(p, map[uint32]ClusterState{4: ClusterUsed},
		ClusterOwner{4: {Entry: "LIVE.TXT", Head: 3, Link: 2}}, outRoot, "test", logger)
	if err != nil {
		t.Fatalf("RestoreAllDeleted: %v", err)
	}
	if sum.Restored != 0 || sum.Failed != 1 {
		t.Errorf("汇总 = %+v, want {Restored:0 Failed:1}", sum)
	}

	logged := buf.String()
	for _, want := range []string{
		`"overlap_cluster":4`,
		`"overlap_owner":"LIVE.TXT"`,
		`"overlap_owner_head":3`,
		`"overlap_owner_link":2`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("日志里缺少 %s\n实际日志:\n%s", want, logged)
		}
	}
	// 逐条失败和最后的汇总都要带；冒号是为了不和 overlap_owner_head 之类的字段混淆。
	for _, field := range []string{`"overlap_owner":`, `"overlap_owner_head":`, `"overlap_owner_link":`} {
		if n := strings.Count(logged, field); n != 2 {
			t.Errorf("%s 出现 %d 次，期望 2 次（候选失败 + 汇总）\n实际日志:\n%s", field, n, logged)
		}
	}
}

// wantLogLine 断言存在某一行同时含全部片段；逐行匹配是因为字段顺序会随日志格式变。
func wantLogLine(t *testing.T, logged string, parts ...string) {
	t.Helper()
	for _, line := range strings.Split(logged, "\n") {
		hit := true
		for _, p := range parts {
			if !strings.Contains(line, p) {
				hit = false
				break
			}
		}
		if hit {
			return
		}
	}
	t.Errorf("日志里没有同时含 %v 的行:\n%s", parts, logged)
}

// TestRestoreAllDeletedLogLevels 盯住三条出路的级别：整份恢复 Info、只捞回前半段 Warn、
// 链头就被占 Error —— Info 可以直接用，Warn 得先看一眼，Error 就是这个文件没了。
func TestRestoreAllDeletedLogLevels(t *testing.T) {
	blk := func(b byte) []byte {
		buf := make([]byte, 512)
		for i := range buf {
			buf[i] = b
		}
		return buf
	}

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	outRoot := filepath.Join(t.TempDir(), "out")
	p := &fakeParser{
		clusterSize: 512,
		firstCid:    2,
		totalCid:    10,
		fat:         map[uint32]uint32{6: 7},
		clusters:    map[uint32][]byte{6: blk(0xAA), 7: blk(0xBB), 8: blk(0xCC)},
		entries: []fsinit.FileEntryItem{
			// 链头 5 就被占住：一个字节都拿不到。
			{Name: "HEAD.TXT", Path: "HEAD.TXT", FirstCluster: 5, DataLength: 512, IsDeleted: true},
			// 第一簇走通了，第二簇 7 被占：能捞回前半段。
			{Name: "PART.TXT", Path: "PART.TXT", FirstCluster: 6, DataLength: 1024, IsDeleted: true},
			// 整条链都空着。
			{Name: "FULL.TXT", Path: "FULL.TXT", FirstCluster: 8, DataLength: 512, IsDeleted: true},
		},
	}
	sum, err := RestoreAllDeleted(p,
		map[uint32]ClusterState{5: ClusterUsed, 6: ClusterFree, 7: ClusterUsed, 8: ClusterFree},
		nil, outRoot, "test", logger)
	if err != nil {
		t.Fatalf("RestoreAllDeleted: %v", err)
	}
	if sum.Restored != 2 || sum.Full != 1 || sum.Partial != 1 || sum.Failed != 1 {
		t.Errorf("汇总 = %+v, want {Restored:2 Full:1 Partial:1 Failed:1}", sum)
	}

	logged := buf.String()
	wantLogLine(t, logged, "level=INFO", "restore file done", "entry=FULL.TXT")
	wantLogLine(t, logged, "level=WARN", "restore file partial", "entry=PART.TXT")
	wantLogLine(t, logged, "level=ERROR", "restore file failed", "entry=HEAD.TXT")

	// 记成 done 或 partial 都会让人以为盘上有东西可捡。
	for _, line := range strings.Split(logged, "\n") {
		if !strings.Contains(line, "HEAD.TXT") {
			continue
		}
		if strings.Contains(line, "restore file done") || strings.Contains(line, "restore file partial") {
			t.Errorf("链头被占不该记成 done/partial: %s", line)
		}
	}

	// 一个都没救回来：补一条 Error 汇总，而不是静默收场。
	var empty strings.Builder
	pOnlyHead := &fakeParser{
		clusterSize: 512,
		firstCid:    2,
		totalCid:    10,
		entries: []fsinit.FileEntryItem{
			{Name: "HEAD.TXT", Path: "HEAD.TXT", FirstCluster: 5, DataLength: 512, IsDeleted: true},
		},
	}
	if _, err := RestoreAllDeleted(pOnlyHead, map[uint32]ClusterState{5: ClusterUsed}, nil,
		filepath.Join(t.TempDir(), "out"), "test", slog.New(slog.NewTextHandler(&empty, nil))); err != nil {
		t.Fatalf("RestoreAllDeleted: %v", err)
	}
	wantLogLine(t, empty.String(), "level=ERROR", "no deleted file restored")

	// 卷上压根没有可恢复的条目：这是正常结束，不该报成错误吓人。
	var none strings.Builder
	pNone := &fakeParser{
		clusterSize: 512,
		firstCid:    2,
		totalCid:    10,
		entries: []fsinit.FileEntryItem{
			{Name: "LIVE.TXT", Path: "LIVE.TXT", FirstCluster: 3, DataLength: 512},
		},
	}
	if _, err := RestoreAllDeleted(pNone, nil, nil, filepath.Join(t.TempDir(), "out"), "test",
		slog.New(slog.NewTextHandler(&none, nil))); err != nil {
		t.Fatalf("RestoreAllDeleted: %v", err)
	}
	wantLogLine(t, none.String(), "level=INFO", "no deleted file candidate")
	if strings.Contains(none.String(), "level=ERROR") || strings.Contains(none.String(), "level=WARN") {
		t.Errorf("没有可恢复条目时不该出现 warn/error:\n%s", none.String())
	}

	// 链走到半路 FAT 就没有下一簇了 —— 已删除的碎片化文件都是这个样子。前半段照救，
	// 但必须记成 partial，并把「为什么短」的现场（断在哪一簇、FAT 项是什么值）摊在日志里。
	var shortBuf strings.Builder
	pShort := &fakeParser{
		clusterSize: 512,
		firstCid:    2,
		totalCid:    10,
		// 没给 FAT[5] 赋值，取值 0，代表这一簇已被释放。
		clusters: map[uint32][]byte{5: blk(0xAA)},
		entries: []fsinit.FileEntryItem{
			{Name: "CUT.TXT", Path: "CUT.TXT", FirstCluster: 5, DataLength: 1024, IsDeleted: true},
		},
	}
	sumShort, err := RestoreAllDeleted(pShort, map[uint32]ClusterState{5: ClusterFree}, nil,
		filepath.Join(t.TempDir(), "out"), "test", slog.New(slog.NewTextHandler(&shortBuf, nil)))
	if err != nil {
		t.Fatalf("RestoreAllDeleted: %v", err)
	}
	if sumShort.Restored != 1 || sumShort.Partial != 1 || sumShort.Failed != 0 || sumShort.Bytes != 512 {
		t.Errorf("汇总 = %+v, want {Restored:1 Partial:1 Failed:0 Bytes:512}", sumShort)
	}
	wantLogLine(t, shortBuf.String(), "level=WARN", "restore file partial", "entry=CUT.TXT")
	wantLogLine(t, shortBuf.String(), "short_bytes=512", "last_cluster=5", "cluster_released=true")
	if strings.Contains(shortBuf.String(), "level=ERROR") {
		t.Errorf("链提前结束是可救的，不该报成 error:\n%s", shortBuf.String())
	}
}
