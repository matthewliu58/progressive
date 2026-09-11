package exfat

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

// 测试镜像的几何参数
const (
	testBytesPerSector = 512
	testSectorsPerClus = 1
	testClusterSize    = testBytesPerSector * testSectorsPerClus
	testFATOffset      = 24
	testFATLength      = 1
	testHeapOffset     = 25
	testClusterCount   = 16
	testVolumeSectors  = 64

	testRootCluster = 3
	testBitmapClus  = 2
	testUpcaseClus  = 5
	testSubDirClus  = 6
	testContigClus  = 8
	testEOC         = 0xFFFFFFFF
)

func clusterByteOffset(cid uint32) int {
	return (testHeapOffset + int(cid-2)*testSectorsPerClus) * testBytesPerSector
}

func fileEntry(typ byte, attrs uint16, secondaryCount int) []byte {
	e := make([]byte, 32)
	e[0] = typ
	e[1] = byte(secondaryCount)
	binary.LittleEndian.PutUint16(e[4:6], attrs)
	return e
}

func streamEntry(typ byte, nameLen int, firstCluster uint32, dataLen uint64, noFatChain bool) []byte {
	e := make([]byte, 32)
	e[0] = typ
	flags := byte(0x01) // AllocationPossible 必须为 1
	if noFatChain {
		flags |= 0x02
	}
	e[1] = flags
	e[3] = byte(nameLen)
	binary.LittleEndian.PutUint64(e[8:16], dataLen) // ValidDataLength
	binary.LittleEndian.PutUint32(e[20:24], firstCluster)
	binary.LittleEndian.PutUint64(e[24:32], dataLen)
	return e
}

func nameEntry(typ byte, runes []uint16) []byte {
	e := make([]byte, 32)
	e[0] = typ
	for i := 0; i < 15; i++ {
		var c uint16
		if i < len(runes) {
			c = runes[i]
		}
		binary.LittleEndian.PutUint16(e[2+i*2:4+i*2], c)
	}
	return e
}

// fileSet 拼一个完整的条目集：File + Stream Extension + N× File Name
func fileSet(name string, attrs uint16, firstCluster uint32, dataLen uint64, deleted, noFatChain bool) []byte {
	fileType, streamType, nameType := byte(entryTypeFile), byte(entryTypeStream), byte(entryTypeFileName)
	if deleted {
		fileType, streamType, nameType = entryTypeFileDeleted, entryTypeStreamDeleted, entryTypeFileNameDeleted
	}

	runes := utf16.Encode([]rune(name))
	nameEnts := (len(runes) + 14) / 15
	if nameEnts == 0 {
		nameEnts = 1
	}

	out := fileEntry(fileType, attrs, 1+nameEnts)
	out = append(out, streamEntry(streamType, len(runes), firstCluster, dataLen, noFatChain)...)
	for i := 0; i < nameEnts; i++ {
		part := runes[i*15:]
		if len(part) > 15 {
			part = part[:15]
		}
		out = append(out, nameEntry(nameType, part)...)
	}
	return out
}

func bitmapEntry(firstCluster uint32, dataLen uint64) []byte {
	e := make([]byte, 32)
	e[0] = entryTypeBitmap
	binary.LittleEndian.PutUint32(e[0x14:0x18], firstCluster)
	binary.LittleEndian.PutUint64(e[0x18:0x20], dataLen)
	return e
}

func upcaseEntry(firstCluster uint32, dataLen uint64) []byte {
	e := make([]byte, 32)
	e[0] = 0x82
	binary.LittleEndian.PutUint32(e[0x14:0x18], firstCluster)
	binary.LittleEndian.PutUint64(e[0x18:0x20], dataLen)
	return e
}

func putEntry(buf []byte, idx *int, ent []byte) {
	copy(buf[(*idx)*32:], ent)
	*idx += len(ent) / 32
}

// buildTestImage 构造一个「根目录跨两个簇 + 目录中间有 0x00 空洞 + 子目录」的镜像。
//
// 目录布局：
//
//	根目录簇3: [0x81][0x82][HELLO.TXT][OLD.DAT(已删除)][0x00 空洞]
//	           [AFTERHOLE.TXT][SUBDIR][0x00 簇尾]
//	根目录簇4: [SECONDCLUSTER.TXT][CONTIG][0x00 ...]
//	SUBDIR(簇6):  [NESTED.BIN]
//	CONTIG(簇8,9, NoFatChain 连续两簇): [C1.TXT] / [C2.TXT]
func buildTestImage() []byte {
	img := make([]byte, testVolumeSectors*testBytesPerSector)

	boot := img[0:512]
	boot[0], boot[1], boot[2] = 0xEB, 0x76, 0x90
	copy(boot[3:11], "EXFAT   ")
	binary.LittleEndian.PutUint64(boot[0x40:0x48], 0)
	binary.LittleEndian.PutUint64(boot[0x48:0x50], testVolumeSectors)
	binary.LittleEndian.PutUint32(boot[0x50:0x54], testFATOffset)
	binary.LittleEndian.PutUint32(boot[0x54:0x58], testFATLength)
	binary.LittleEndian.PutUint32(boot[0x58:0x5C], testHeapOffset)
	binary.LittleEndian.PutUint32(boot[0x5C:0x60], testClusterCount)
	binary.LittleEndian.PutUint32(boot[0x60:0x64], testRootCluster)
	binary.LittleEndian.PutUint32(boot[0x64:0x68], 0x12345678)
	binary.LittleEndian.PutUint16(boot[0x68:0x6A], 0x0100)
	binary.LittleEndian.PutUint16(boot[0x6A:0x6C], 0)
	boot[0x6C] = 9 // BytesPerSectorShift = 512B
	boot[0x6D] = 0 // SectorsPerClusterShift = 1 簇
	boot[0x6E] = 1
	boot[0x6F] = 0x80
	boot[0x70] = 0xFF
	binary.LittleEndian.PutUint16(boot[0x1FE:0x200], 0xAA55)

	// FAT：根目录 3 -> 4 -> EOC；CONTIG 故意不设链（8 -> 0），用来验证 NoFatChain
	fatOff := testFATOffset * testBytesPerSector
	setFat := func(cid, val uint32) {
		binary.LittleEndian.PutUint32(img[fatOff+int(cid)*4:], val)
	}
	setFat(0, 0xFFFFFFF8)
	setFat(1, 0xFFFFFFFF)
	setFat(testBitmapClus, testEOC)
	setFat(testRootCluster, 4)
	setFat(4, testEOC)
	setFat(testUpcaseClus, testEOC)
	setFat(testSubDirClus, testEOC)
	setFat(testContigClus, 0) // 注意：故意为 0
	setFat(testContigClus+1, testEOC)

	// 分配位图：标记所有用到的簇
	bitmap := make([]byte, testClusterSize)
	for _, cid := range []uint32{2, 3, 4, 5, 6, 8, 9} {
		b := cid - 2
		bitmap[b/8] |= 1 << (b % 8)
	}
	copy(img[clusterByteOffset(testBitmapClus):], bitmap)

	// 根目录第一个簇
	root1 := make([]byte, testClusterSize)
	idx := 0
	putEntry(root1, &idx, bitmapEntry(testBitmapClus, testClusterSize))
	putEntry(root1, &idx, upcaseEntry(testUpcaseClus, testClusterSize))
	putEntry(root1, &idx, fileSet("HELLO.TXT", 0x20, 0, 0, false, false))
	putEntry(root1, &idx, fileSet("OLD.DAT", 0x20, 0, 0, true, false))
	idx++ // 故意留一个 0x00 空洞
	putEntry(root1, &idx, fileSet("AFTERHOLE.TXT", 0x20, 0, 0, false, false))
	putEntry(root1, &idx, fileSet("SUBDIR", attrDirectory, testSubDirClus, testClusterSize, false, false))
	if idx != 15 {
		panic("root1 条目数不对")
	}
	// idx 15 保持 0x00：簇尾是空的，但根目录链还有下一个簇
	copy(img[clusterByteOffset(testRootCluster):], root1)

	// 根目录第二个簇
	root2 := make([]byte, testClusterSize)
	idx = 0
	putEntry(root2, &idx, fileSet("SECONDCLUSTER.TXT", 0x20, 0, 0, false, false))
	putEntry(root2, &idx, fileSet("CONTIG", attrDirectory, testContigClus, 2*testClusterSize, false, true))
	copy(img[clusterByteOffset(4):], root2)

	// SUBDIR
	sub := make([]byte, testClusterSize)
	idx = 0
	putEntry(sub, &idx, fileSet("NESTED.BIN", 0x20, 0, 0, false, false))
	copy(img[clusterByteOffset(testSubDirClus):], sub)

	// CONTIG 的两个连续簇
	c1 := make([]byte, testClusterSize)
	idx = 0
	putEntry(c1, &idx, fileSet("C1.TXT", 0x20, 0, 0, false, false))
	copy(img[clusterByteOffset(testContigClus):], c1)

	c2 := make([]byte, testClusterSize)
	idx = 0
	putEntry(c2, &idx, fileSet("C2.TXT", 0x20, 0, 0, false, false))
	copy(img[clusterByteOffset(testContigClus+1):], c2)

	return img
}

func TestLoadDirectoryTree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.img")
	if err := os.WriteFile(path, buildTestImage(), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()

	p := NewExFatParser("test", nil)
	if err := p.Load(f); err != nil {
		t.Fatalf("Load: %v", err)
	}

	wantPaths := []string{
		"HELLO.TXT",
		"OLD.DAT",
		"AFTERHOLE.TXT",
		"SUBDIR",
		"SUBDIR/NESTED.BIN",
		"SECONDCLUSTER.TXT",
		"CONTIG",
		"CONTIG/C1.TXT",
		"CONTIG/C2.TXT",
	}

	gotPaths := make([]string, 0, len(p.info.Files))
	for _, fi := range p.info.Files {
		gotPaths = append(gotPaths, fi.Path)
	}

	if len(gotPaths) != len(wantPaths) {
		t.Fatalf("条目数不对\n got: %v\nwant: %v", gotPaths, wantPaths)
	}
	for i := range wantPaths {
		if gotPaths[i] != wantPaths[i] {
			t.Fatalf("第 %d 个条目不匹配: got %q want %q\n got: %v", i, gotPaths[i], wantPaths[i], gotPaths)
		}
	}

	// 位图和 FAT 也要照常读出来
	if p.info.Bitmap.FirstCluster != testBitmapClus {
		t.Errorf("bitmap first cluster = %d, want %d", p.info.Bitmap.FirstCluster, testBitmapClus)
	}
	if p.info.Bitmap.DataLength != testClusterSize {
		t.Errorf("bitmap data length = %d, want %d", p.info.Bitmap.DataLength, testClusterSize)
	}
	if len(p.info.FAT) != testClusterCount+2 {
		t.Errorf("fat entries = %d, want %d", len(p.info.FAT), testClusterCount+2)
	}
}

func TestLoadMarksDeletedAndDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.img")
	if err := os.WriteFile(path, buildTestImage(), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()

	p := NewExFatParser("test", nil)
	if err := p.Load(f); err != nil {
		t.Fatalf("Load: %v", err)
	}

	byPath := map[string]FileInfo{}
	for _, fi := range p.info.Files {
		byPath[fi.Path] = fi
	}

	if fi, ok := byPath["OLD.DAT"]; !ok {
		t.Fatal("没找到 OLD.DAT")
	} else if !fi.IsDeleted {
		t.Error("OLD.DAT 应该标记为已删除")
	}

	for _, name := range []string{"HELLO.TXT", "AFTERHOLE.TXT", "SECONDCLUSTER.TXT"} {
		if fi, ok := byPath[name]; !ok {
			t.Errorf("没找到 %s", name)
		} else if fi.IsDeleted {
			t.Errorf("%s 不该被标记为已删除", name)
		}
	}

	for _, name := range []string{"SUBDIR", "CONTIG"} {
		if fi, ok := byPath[name]; !ok {
			t.Errorf("没找到 %s", name)
		} else if !fi.isDir() {
			t.Errorf("%s 应该被识别为目录", name)
		}
	}

	// CONTIG 是 NoFatChain（连续分配）的目录，FAT[8] 故意留 0，
	// 能读到 C2.TXT 说明确实是按「firstCluster+i」算的，没有去查 FAT。
	if fi, ok := byPath["CONTIG"]; !ok {
		t.Fatal("没找到 CONTIG")
	} else if !fi.NoFatChain {
		t.Error("CONTIG 的 NoFatChain 应为 true")
	}
	if _, ok := byPath["CONTIG/C2.TXT"]; !ok {
		t.Error("没找到 CONTIG/C2.TXT —— NoFatChain 连续簇没读全")
	}
}
