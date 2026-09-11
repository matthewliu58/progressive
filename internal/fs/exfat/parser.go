package exfat

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	fsinit "progrescarve/internal/fs/finit"
	"strconv"
	"unicode/utf16"
)

const (
	sectorSize               = 512
	entryTypeEnd             = 0x00
	entryTypeFile            = 0x85
	entryTypeFileDeleted     = 0x05
	entryTypeStream          = 0xC0
	entryTypeStreamDeleted   = 0x40
	entryTypeFileName        = 0xC1
	entryTypeFileNameDeleted = 0x41
	entryTypeBitmap          = 0x81

	// attrDirectory 是 File 条目 Attributes 字段的「这是目录」位（bit 4）
	attrDirectory = 0x0010

	// maxDirDepth 防止损坏卷上的目录互相指向导致无限递归
	maxDirDepth = 64
)

type BootInfo struct {
	PartitionOffset      uint64 `json:"partition_offset"`
	VolumeLength         uint64 `json:"volume_length"`
	FATOffset            uint32 `json:"fat_offset_sector"`
	FATLength            uint32 `json:"fat_length_sector"`
	ClusterHeapOffset    uint32 `json:"cluster_heap_offset_sector"`
	ClusterCount         uint32 `json:"cluster_count"`
	RootDirectoryCluster uint32 `json:"root_dir_cluster"`
	VolumeSerialNumber   uint32 `json:"serial_number"`
	FileSystemRevision   uint16 `json:"fs_revision"`
	VolumeFlags          uint16 `json:"volume_flags"`
	BytesPerSector       uint32 `json:"bytes_per_sector"`
	SectorsPerCluster    uint32 `json:"sectors_per_cluster"`
	NumberOfFATs         uint8  `json:"number_of_fats"`
	DriveSelect          uint8  `json:"drive_select"`
	PercentInUse         uint8  `json:"percent_in_use"`
	BootSignature        uint16 `json:"boot_signature"`
}

type BitmapInfo struct {
	FirstCluster uint32 `json:"first_cluster"`
	DataLength   uint64 `json:"data_length"`
}

type FileInfo struct {
	Name            string `json:"name"`
	Path            string `json:"path"`
	Attributes      uint16 `json:"attributes"`
	FirstCluster    uint32 `json:"first_cluster"`
	DataLength      uint64 `json:"data_length"`
	ValidDataLength uint64 `json:"valid_data_length"`
	NameLength      uint8  `json:"name_length"`
	NameHash        uint16 `json:"name_hash"`
	NoFatChain      bool   `json:"no_fat_chain"`
	IsDeleted       bool   `json:"is_deleted"`
}

type ExFATInfo struct {
	Boot        BootInfo   `json:"boot"`
	Bitmap      BitmapInfo `json:"bitmap"`
	BitmapData  []byte     `json:"-"`
	Files       []FileInfo `json:"files"`
	ClusterSize uint64     `json:"cluster_size"`
	FAT         []uint32   `json:"fat"`
}

// exFatMetaFile 在 FileInfo 上补一个由 Attributes 推导出的 is_dir
type exFatMetaFile struct {
	FileInfo
	IsDir bool `json:"is_dir"`
}

// exFatMetaDump 是 DebugPrintMeta 打印的 JSON 结构
type exFatMetaDump struct {
	Boot   BootInfo        `json:"boot"`
	Bitmap BitmapInfo      `json:"bitmap"`
	Files  []exFatMetaFile `json:"files"`

	ClusterSize     uint64  `json:"cluster_size"`
	BitmapDataBytes int     `json:"bitmap_data_bytes"`
	FATEntries      int     `json:"fat_entries"`
	FAT             fatDump `json:"fat"`
	EntryCount      int     `json:"entry_count"`
	LiveCount       int     `json:"live_count"`
	DeletedCount    int     `json:"deleted_count"`
	DirCount        int     `json:"dir_count"`
}

// fatDumpEntries 是 DebugPrintMeta 里输出 FAT 的项数。整表 390 万项（1TB 卷），
// 全铺出来十几 MB，一行日志直接废掉；前 1000 项够看清簇堆开头的排布。
const fatDumpEntries = 1000

// fatDump 把 FAT 前若干项写成「簇号: 值」的 JSON 对象。
//
// 不用 map[uint32]uint32：encoding/json 会先把整数键转成字符串、再按字典序排，
// 于是 "10" 插在 "100" 前面，簇号顺序就废了；这里自己按簇号升序逐个写出。
type fatDump []uint32

func (f fatDump) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.Grow(8 + len(f)*12)
	b.WriteByte('{')
	for i, v := range f {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strconv.FormatUint(uint64(i), 10))
		b.WriteString(`":`)
		b.WriteString(strconv.FormatUint(uint64(v), 10))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// ExFatParser 实现 FileSystemParser
type ExFatParser struct {
	info   *ExFATInfo
	f      *os.File
	logger *slog.Logger
	pre    string
}

func NewExFatParser(pre string, logger *slog.Logger) *ExFatParser {
	return &ExFatParser{logger: logger, pre: pre}
}

func (p *ExFatParser) Load(f *os.File) error {
	p.f = f

	bootSector := make([]byte, sectorSize)
	_, err := f.ReadAt(bootSector, 0)
	if err != nil {
		return fmt.Errorf("read boot sector: %w", err)
	}

	boot := BootInfo{
		PartitionOffset:      binary.LittleEndian.Uint64(bootSector[0x40:0x48]),
		VolumeLength:         binary.LittleEndian.Uint64(bootSector[0x48:0x50]),
		FATOffset:            binary.LittleEndian.Uint32(bootSector[0x50:0x54]),
		FATLength:            binary.LittleEndian.Uint32(bootSector[0x54:0x58]),
		ClusterHeapOffset:    binary.LittleEndian.Uint32(bootSector[0x58:0x5C]),
		ClusterCount:         binary.LittleEndian.Uint32(bootSector[0x5C:0x60]),
		RootDirectoryCluster: binary.LittleEndian.Uint32(bootSector[0x60:0x64]),
		VolumeSerialNumber:   binary.LittleEndian.Uint32(bootSector[0x64:0x68]),
		FileSystemRevision:   binary.LittleEndian.Uint16(bootSector[0x68:0x6A]),
		VolumeFlags:          binary.LittleEndian.Uint16(bootSector[0x6A:0x6C]),
		BytesPerSector:       uint32(1) << bootSector[0x6C],
		SectorsPerCluster:    uint32(1) << bootSector[0x6D],
		NumberOfFATs:         bootSector[0x6E],
		DriveSelect:          bootSector[0x6F],
		PercentInUse:         bootSector[0x70],
		BootSignature:        binary.LittleEndian.Uint16(bootSector[0x1FE:0x200]),
	}

	clusterSize := uint64(boot.BytesPerSector) * uint64(boot.SectorsPerCluster)

	fatSize := uint64(boot.FATLength) * uint64(boot.BytesPerSector)
	fatData := make([]byte, fatSize)
	fatOffsetBytes := uint64(boot.FATOffset) * uint64(boot.BytesPerSector)
	_, err = f.ReadAt(fatData, int64(fatOffsetBytes))
	if err != nil {
		return fmt.Errorf("read FAT: %w", err)
	}

	fatEntryCount := boot.ClusterCount + 2
	fatTable := make([]uint32, fatEntryCount)
	maxFatOffset := uint64(len(fatData))
	for cluster := uint32(2); cluster < fatEntryCount; cluster++ {
		off := uint64(cluster) * 4
		if off+4 > maxFatOffset {
			break
		}
		val := binary.LittleEndian.Uint32(fatData[off : off+4])
		fatTable[cluster] = val & 0x0FFFFFFF
	}

	// 根目录本身是一条 FAT 簇链，要整条读完——只读第一个簇的话，
	// 后面簇里的条目（通常是较新的、还活着的那些）全都看不到。
	// 根目录没有流扩展条目，dataLength 未知，传 0 表示一直读到链尾。
	rootData, err := readDirData(f, &boot, fatTable, clusterSize, boot.RootDirectoryCluster, 0, false)
	if err != nil {
		return fmt.Errorf("read root dir failed: %w", err)
	}

	bitmapCluster, bitmapLen, rootEntries := parseDirectoryEntries(rootData)

	// 收下根目录下的条目，并对标记为目录的条目递归展开
	files := make([]FileInfo, 0, len(rootEntries))
	visitedDirs := map[uint32]bool{boot.RootDirectoryCluster: true}
	for i := range rootEntries {
		ent := rootEntries[i]
		ent.Path = ent.Name
		files = append(files, ent)

		if !ent.isDir() || ent.FirstCluster < 2 || visitedDirs[ent.FirstCluster] {
			continue
		}
		visitedDirs[ent.FirstCluster] = true
		if err := walkDirectory(f, &boot, fatTable, clusterSize, ent, 1, visitedDirs, &files); err != nil {
			// 单个子目录读失败不该让整个卷加载失败
			p.warn("exfat: walk sub directory failed", ent.Path, err)
		}
	}

	bitmapBytes, err := readAllocationBitmap(f, &boot, bitmapCluster, bitmapLen, clusterSize)
	if err != nil {
		return fmt.Errorf("read allocation bitmap failed: %w", err)
	}

	p.info = &ExFATInfo{
		Boot:        boot,
		Bitmap:      BitmapInfo{FirstCluster: bitmapCluster, DataLength: bitmapLen},
		BitmapData:  bitmapBytes,
		Files:       files,
		ClusterSize: clusterSize,
		FAT:         fatTable,
	}
	return nil
}

func (p *ExFatParser) GetClusterFSInfo(cid uint32) (alloc bool, fatNext uint32, err error) {
	alloc, err = IsClusterAllocated(cid, p.info.BitmapData)
	if err != nil {
		return false, 0, err
	}
	fatNext = p.info.FAT[cid]
	return alloc, fatNext, nil
}

func IsClusterAllocated(cid uint32, bitmapData []byte) (bool, error) {
	if cid < 2 {
		return false, fmt.Errorf("cluster id must >=2")
	}
	bitIdx := cid - 2
	byteIdx := bitIdx / 8
	bitPos := bitIdx % 8
	if int(byteIdx) >= len(bitmapData) {
		return false, fmt.Errorf("cluster %d out of bitmap range", cid)
	}
	b := bitmapData[byteIdx]
	return (b & (1 << bitPos)) != 0, nil
}

func (p *ExFatParser) ListAllFileEntries() ([]fsinit.FileEntryItem, error) {
	var out []fsinit.FileEntryItem
	for _, fi := range p.info.Files {
		out = append(out, fsinit.FileEntryItem{
			Name:         fi.Name,
			Path:         fi.Path,
			FirstCluster: fi.FirstCluster,
			DataLength:   fi.DataLength,
			ValidLength:  fi.ValidDataLength,
			IsDeleted:    fi.IsDeleted,
			IsDir:        fi.isDir(),
			NoFatChain:   fi.NoFatChain,
		})
	}
	return out, nil
}

func (p *ExFatParser) ClusterHeapRange() (clusterSize uint64, firstCluster uint32, totalCluster uint32) {
	return p.info.ClusterSize, 2, p.info.Boot.ClusterCount
}

// SystemClusters 返回簇堆里承载文件系统公共信息（分配位图、根目录）的簇号。
// 这些簇不存文件内容，却实实在在占着簇，恢复时不能当成可回收的空闲簇。
//
// up-case 表当前没有解析，无法指名；它会被归入「其他」状态。恢复只关心文件
// 内容，所以不影响判断。
func (p *ExFatParser) SystemClusters() []uint32 {
	if p.info == nil {
		return nil
	}

	var out []uint32

	// 分配位图：DataLength 是字节数，可能跨多个簇。
	if c := p.info.Bitmap.FirstCluster; c >= 2 {
		count := (p.info.Bitmap.DataLength + p.info.ClusterSize - 1) / p.info.ClusterSize
		if count == 0 {
			count = 1
		}
		for i := uint64(0); i < count; i++ {
			out = append(out, c+uint32(i))
		}
	}

	// 根目录：一整条簇链，可能跨多个簇。
	fat := p.info.FAT
	seen := make(map[uint32]struct{})
	for cid := p.info.Boot.RootDirectoryCluster; cid >= 2 && int(cid) < len(fat); {
		if _, dup := seen[cid]; dup {
			break
		}
		seen[cid] = struct{}{}
		out = append(out, cid)

		next := fatNextCluster(fat, cid)
		if next == 0 {
			break
		}
		cid = next
	}

	return out
}

func (p *ExFatParser) ReadCluster(cid uint32) ([]byte, error) {
	boot := p.info.Boot
	off, err := clusterOffset(&boot, cid, p.info.ClusterSize)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, p.info.ClusterSize)
	_, err = p.f.ReadAt(buf, int64(off))
	return buf, err
}

func (p *ExFatParser) DebugPrintMeta() {
	logger := p.logger
	if logger == nil {
		logger = slog.Default()
	}

	if p.info == nil {
		logger.Warn("exfat: meta not loaded", slog.Any("pre", p.pre))
		return
	}

	info := p.info

	dump := exFatMetaDump{
		Boot:            info.Boot,
		Bitmap:          info.Bitmap,
		Files:           make([]exFatMetaFile, 0, len(info.Files)),
		ClusterSize:     info.ClusterSize,
		BitmapDataBytes: len(info.BitmapData),
		FATEntries:      len(info.FAT),
		EntryCount:      len(info.Files),
	}
	for i := range info.Files {
		f := info.Files[i]
		if f.IsDeleted {
			dump.DeletedCount++
		} else {
			dump.LiveCount++
		}
		if f.isDir() {
			dump.DirCount++
		}
		dump.Files = append(dump.Files, exFatMetaFile{FileInfo: f, IsDir: f.isDir()})
	}

	// FAT 只带前 fatDumpEntries 项：它是按簇号铺的切片，3.9M 项全塞进日志会撑爆一行。
	// 条目数本身由 FATEntries 给出，剩下的簇要查就直接读 FAT。
	n := len(info.FAT)
	if n > fatDumpEntries {
		n = fatDumpEntries
	}
	dump.FAT = fatDump(info.FAT[:n])

	// 整块 marshal 成 JSON 再交给 logger，不要拆成几十个 slog 字段。
	// 这里传 json.RawMessage 而不是 string：RawMessage 实现了 json.Marshaler，
	// JSONHandler 会把它原样嵌进日志行；换成 string 会被再加一层引号转义，反而更难读。
	b, err := json.Marshal(dump)
	if err != nil {
		logger.Error("exfat: marshal meta failed", slog.Any("pre", p.pre), slog.Any("err", err))
		return
	}

	logger.Debug("exfat meta", slog.String("pre", p.pre), slog.Any("meta", json.RawMessage(b)))
}

func clusterOffset(boot *BootInfo, cluster uint32, clusterSize uint64) (uint64, error) {
	if cluster < 2 {
		return 0, fmt.Errorf("invalid cluster %d", cluster)
	}
	heapStart := uint64(boot.ClusterHeapOffset) * uint64(boot.BytesPerSector)
	offset := heapStart + uint64(cluster-2)*clusterSize
	return offset, nil
}

// parseDirectoryEntries 解析一段目录数据，返回分配位图信息（只有根目录有）和其中的所有条目。
//
// 关于 0x00：规范里它是「目录结束」标记，但卷大量增删之后目录中间是会出现 0x00
// 空洞的（目录扩过簇、旧条目被清掉等）。所以这里只有确认「从这个位置到缓冲区末尾
// 全是 0」才当作真的结束，否则当成一个空槽跳过继续扫——直接 break 的话，空洞后面
// 的条目会整批丢掉。
func parseDirectoryEntries(data []byte) (uint32, uint64, []FileInfo) {
	var bitmapCluster uint32
	var bitmapLength uint64
	var outFiles []FileInfo
	offset := 0
	dlen := len(data)

	// 最后一个非零字节的位置，用来 O(1) 判断「从某处到末尾是否全 0」
	lastNonZero := 0
	for i := dlen - 1; i >= 0; i-- {
		if data[i] != 0 {
			lastNonZero = i + 1
			break
		}
	}

	for offset+32 <= dlen {
		ent := data[offset : offset+32]
		typ := ent[0]
		if typ == entryTypeEnd {
			if offset >= lastNonZero {
				break
			}
			offset += 32
			continue
		}
		switch typ {
		case entryTypeBitmap:
			bitmapCluster = binary.LittleEndian.Uint32(ent[0x14:0x18])
			bitmapLength = binary.LittleEndian.Uint64(ent[0x18:0x20])
			offset += 32
		case entryTypeFile, entryTypeFileDeleted:
			sc := int(ent[1])
			if sc > 128 {
				offset += 32
				continue
			}
			setSize := 32 * (sc + 1)
			if offset+setSize > dlen {
				offset += 32
				continue
			}
			setBuf := data[offset : offset+setSize]
			fi, ok := parseFileEntrySet(setBuf, typ == entryTypeFileDeleted)
			if ok {
				outFiles = append(outFiles, fi)
			}
			offset += setSize
		default:
			offset += 32
		}
	}
	return bitmapCluster, bitmapLength, outFiles
}

func parseFileEntrySet(set []byte, isDeleted bool) (FileInfo, bool) {
	var fi FileInfo
	fi.IsDeleted = isDeleted
	if len(set) < 64 {
		return fi, false
	}
	fileEnt := set[0:32]
	if fileEnt[0] != entryTypeFile && fileEnt[0] != entryTypeFileDeleted {
		return fi, false
	}
	fi.Attributes = binary.LittleEndian.Uint16(fileEnt[4:6])
	var streamEnt []byte
	var nameEnts [][]byte
	for off := 32; off+32 <= len(set); off += 32 {
		e := set[off : off+32]
		switch e[0] {
		case entryTypeStream, entryTypeStreamDeleted:
			streamEnt = e
		case entryTypeFileName, entryTypeFileNameDeleted:
			nameEnts = append(nameEnts, e)
		}
	}
	if streamEnt == nil {
		return fi, false
	}
	fi.NoFatChain = (streamEnt[1] & 0x02) != 0
	fi.NameLength = streamEnt[3]
	fi.NameHash = binary.LittleEndian.Uint16(streamEnt[4:6])
	fi.ValidDataLength = binary.LittleEndian.Uint64(streamEnt[8:16])
	fi.FirstCluster = binary.LittleEndian.Uint32(streamEnt[20:24])
	fi.DataLength = binary.LittleEndian.Uint64(streamEnt[24:32])

	var runes []uint16
	for _, ne := range nameEnts {
		for i := range 15 {
			pos := 2 + i*2
			if pos+2 > len(ne) {
				break
			}
			c := binary.LittleEndian.Uint16(ne[pos : pos+2])
			if c == 0 {
				break
			}
			runes = append(runes, c)
		}
	}
	if int(fi.NameLength) < len(runes) {
		runes = runes[:fi.NameLength]
	}
	fi.Name = string(utf16.Decode(runes))
	return fi, true
}

func readAllocationBitmap(f *os.File, boot *BootInfo, bitmapCluster uint32, bitmapLen uint64, clusterSize uint64) ([]byte, error) {
	if bitmapCluster < 2 {
		return nil, fmt.Errorf("bitmap cluster invalid: %d", bitmapCluster)
	}
	off, err := clusterOffset(boot, bitmapCluster, clusterSize)
	if err != nil {
		return nil, err
	}
	secSize := uint64(boot.BytesPerSector)
	readLen := ((bitmapLen + secSize - 1) / secSize) * secSize
	buf := make([]byte, readLen)
	_, err = f.ReadAt(buf, int64(off))
	if err != nil {
		return nil, fmt.Errorf("read allocation bitmap data: %w", err)
	}
	return buf[:bitmapLen], nil
}

func (f FileInfo) isDir() bool {
	return f.Attributes&attrDirectory != 0
}

func (p *ExFatParser) warn(msg, path string, err error) {
	if p.logger == nil {
		return
	}
	p.logger.Warn(msg, slog.String("pre", p.pre), slog.String("path", path), slog.Any("err", err))
}

// readDirData 把一个目录的完整内容读出来——沿 FAT 簇链读，而不是只读一个簇。
//
// dataLength 传 0 表示长度未知（根目录没有对应的流扩展条目），此时一直读到
// FAT 链结束。noFatChain 为 true 表示该目录数据是连续分配的，簇号为
// firstCluster+i，不需要查 FAT。
func readDirData(f *os.File, boot *BootInfo, fat []uint32, clusterSize uint64,
	firstCluster uint32, dataLength uint64, noFatChain bool) ([]byte, error) {

	if firstCluster < 2 {
		return nil, nil
	}

	// 该目录最多有多少个簇：长度已知就按长度算，否则以 FAT 表长为上限
	maxClusters := uint64(len(fat))
	if dataLength > 0 {
		if n := (dataLength + clusterSize - 1) / clusterSize; n > 0 && n < maxClusters {
			maxClusters = n
		}
	}
	if maxClusters == 0 {
		maxClusters = 1
	}

	out := make([]byte, 0, clusterSize)
	seen := make(map[uint32]struct{})
	cluster := firstCluster

	for i := uint64(0); i < maxClusters; i++ {
		if noFatChain && i > 0 {
			cluster = firstCluster + uint32(i)
		}
		if cluster < 2 || int(cluster) >= len(fat) {
			break
		}
		if _, dup := seen[cluster]; dup {
			break // FAT 链成环，停
		}
		seen[cluster] = struct{}{}

		off, err := clusterOffset(boot, cluster, clusterSize)
		if err != nil {
			break
		}
		buf := make([]byte, clusterSize)
		if _, err := f.ReadAt(buf, int64(off)); err != nil {
			return out, fmt.Errorf("read dir cluster %d: %w", cluster, err)
		}
		out = append(out, buf...)

		if noFatChain {
			continue
		}
		cluster = fatNextCluster(fat, cluster)
		if cluster == 0 {
			break
		}
	}
	return out, nil
}

// fatNextCluster 返回 FAT 中 cluster 的下一个簇号；0 表示链结束（EOC 或非法值）。
func fatNextCluster(fat []uint32, cluster uint32) uint32 {
	if int(cluster) >= len(fat) {
		return 0
	}
	next := fat[cluster]
	if next < 2 || int(next) >= len(fat) {
		return 0
	}
	return next
}

// walkDirectory 递归展开一个子目录，把它内部的条目（含更深层的）追加到 out。
// dir 是父目录里代表本目录的那条条目，它已经记在 out 里了，这里只处理它的内部。
func walkDirectory(f *os.File, boot *BootInfo, fat []uint32, clusterSize uint64,
	dir FileInfo, depth int, visited map[uint32]bool, out *[]FileInfo) error {

	if depth > maxDirDepth {
		return nil
	}

	data, err := readDirData(f, boot, fat, clusterSize, dir.FirstCluster, dir.DataLength, dir.NoFatChain)
	if err != nil {
		return err
	}
	_, _, entries := parseDirectoryEntries(data)

	for i := range entries {
		ent := entries[i]
		ent.Path = dir.Path + "/" + ent.Name
		*out = append(*out, ent)

		if !ent.isDir() || ent.FirstCluster < 2 || visited[ent.FirstCluster] {
			continue
		}
		visited[ent.FirstCluster] = true
		// 单个子目录读失败不影响其它分支
		_ = walkDirectory(f, boot, fat, clusterSize, ent, depth+1, visited, out)
	}
	return nil
}
