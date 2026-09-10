package exfat

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	fsinit "progrescarve/internal/fs/finit"
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
	FAT         []uint32   `json:"-"`
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

	rootOff, err := clusterOffset(&boot, boot.RootDirectoryCluster, clusterSize)
	if err != nil {
		return err
	}
	rootData := make([]byte, clusterSize)
	_, err = f.ReadAt(rootData, int64(rootOff))
	if err != nil {
		return fmt.Errorf("read root dir cluster failed: %w", err)
	}

	bitmapCluster, bitmapLen, files := parseRootDirectory(rootData)
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
			FirstCluster: fi.FirstCluster,
			DataLength:   fi.DataLength,
			ValidLength:  fi.ValidDataLength,
			IsDeleted:    fi.IsDeleted,
			NoFatChain:   fi.NoFatChain,
		})
	}
	return out, nil
}

func (p *ExFatParser) ClusterHeapRange() (clusterSize uint64, firstCluster uint32, totalCluster uint32) {
	return p.info.ClusterSize, 2, p.info.Boot.ClusterCount
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
	b := info.Boot

	logger.Debug("exfat boot",
		slog.Any("pre", p.pre),
		slog.Any("partition_offset", b.PartitionOffset),
		slog.Any("volume_length", b.VolumeLength),
		slog.Any("fat_offset_sector", b.FATOffset),
		slog.Any("fat_length_sector", b.FATLength),
		slog.Any("cluster_heap_offset_sector", b.ClusterHeapOffset),
		slog.Any("cluster_count", b.ClusterCount),
		slog.Any("root_dir_cluster", b.RootDirectoryCluster),
		slog.Any("serial_number", b.VolumeSerialNumber),
		slog.Any("fs_revision", b.FileSystemRevision),
		slog.Any("volume_flags", b.VolumeFlags),
		slog.Any("bytes_per_sector", b.BytesPerSector),
		slog.Any("sectors_per_cluster", b.SectorsPerCluster),
		slog.Any("number_of_fats", b.NumberOfFATs),
		slog.Any("drive_select", b.DriveSelect),
		slog.Any("percent_in_use", b.PercentInUse),
		slog.Any("boot_signature", b.BootSignature),
	)

	logger.Debug("exfat layout",
		slog.Any("pre", p.pre),
		slog.Any("cluster_size", info.ClusterSize),
		slog.Any("bitmap_first_cluster", info.Bitmap.FirstCluster),
		slog.Any("bitmap_data_length", info.Bitmap.DataLength),
		slog.Int("bitmap_data_bytes", len(info.BitmapData)),
		slog.Int("fat_entries", len(info.FAT)),
		slog.Int("file_count", len(info.Files)),
	)

	//logger.Debug("exfat fat table", slog.Any("pre", p.pre), slog.Any("fat", info.FAT))
	//logger.Debug("exfat allocation bitmap", slog.Any("pre", p.pre), slog.Any("bitmap_data", info.BitmapData))

	for i, f := range info.Files {
		logger.Debug("exfat file",
			slog.Any("pre", p.pre),
			slog.Int("index", i),
			slog.String("name", f.Name),
			slog.Any("attributes", f.Attributes),
			slog.Any("first_cluster", f.FirstCluster),
			slog.Any("data_length", f.DataLength),
			slog.Any("valid_data_length", f.ValidDataLength),
			slog.Any("name_length", f.NameLength),
			slog.Any("name_hash", f.NameHash),
			slog.Any("no_fat_chain", f.NoFatChain),
			slog.Any("is_deleted", f.IsDeleted),
		)
	}
}

func clusterOffset(boot *BootInfo, cluster uint32, clusterSize uint64) (uint64, error) {
	if cluster < 2 {
		return 0, fmt.Errorf("invalid cluster %d", cluster)
	}
	heapStart := uint64(boot.ClusterHeapOffset) * uint64(boot.BytesPerSector)
	offset := heapStart + uint64(cluster-2)*clusterSize
	return offset, nil
}

func parseRootDirectory(data []byte) (uint32, uint64, []FileInfo) {
	var bitmapCluster uint32
	var bitmapLength uint64
	var outFiles []FileInfo
	offset := 0
	dlen := len(data)

	for offset+32 <= dlen {
		ent := data[offset : offset+32]
		typ := ent[0]
		if typ == entryTypeEnd {
			break
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
