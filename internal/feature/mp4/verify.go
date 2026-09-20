package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// structureProbeSize 是结构校验最多看多少字节。
//
// 顶层 box 头都在文件最前面（ftyp、moov、mdat 的头），没必要把后面几百 MB 的媒体
// 数据也读一遍 —— 校验要便宜，否则没人敢开着它跑全盘恢复。
const structureProbeSize = 1 << 20

// StructureError 表示文件开头这套 box 结构说不通。
type StructureError struct {
	Offset int64  // 在哪个字节上发现的
	Why    string // 具体哪条判据没过
}

func (e *StructureError) Error() string {
	return fmt.Sprintf("mp4 box structure invalid at offset %d: %s", e.Offset, e.Why)
}

// CheckStructure 检查 r 的开头是不是一套说得通的顶层 box 结构。
//
// 能验到什么程度要讲清楚：mdat 内部没有同步结构，**中间被换掉几簇是查不出来的**。
// 它能挡住的是另外两类最常见的错：
//   - 第一簇就拼错了（那开头根本不是 ftyp）；
//   - 拼进来的第一簇是别人的数据（box 类型码不是可打印的四字符码、长度说不通）。
//
// 判据：
//   - 第一个 box 必须是 ftyp —— MP4 / MOV / M4A 这一家子都这样；
//   - 每个 box 的长度不小于头长（8），类型码是四个可打印字符；
//   - size == 0 表示「到文件尾」，size == 1 表示后面跟 8 字节的 64 位长度，按规范走；
//   - 链走出探测窗口就停 —— 那是 mdat 太大，正常，不再往下验。
func CheckStructure(r io.Reader) error {
	buf := make([]byte, structureProbeSize)
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	if n < boxHeader {
		return &StructureError{Offset: 0, Why: "file shorter than one box header"}
	}
	buf = buf[:n]

	if string(buf[4:8]) != "ftyp" {
		return &StructureError{Offset: 0, Why: fmt.Sprintf("first box is %q, want %q", buf[4:8], "ftyp")}
	}

	off := int64(0)
	for off+boxHeader <= int64(len(buf)) {
		typ := string(buf[off+4 : off+8])
		if !boxTypeLooksSane(typ) {
			return &StructureError{Offset: off, Why: fmt.Sprintf("box type %q is not four printable chars", typ)}
		}

		size := int64(binary.BigEndian.Uint32(buf[off:]))
		switch size {
		case 0:
			return nil // 到文件尾，正常结束
		case 1:
			// 64 位长度：紧跟在类型码后面 8 字节。
			if off+16 > int64(len(buf)) {
				return nil // 超出探测窗口，验到这儿为止
			}
			size = int64(binary.BigEndian.Uint64(buf[off+8:]))
			if size < 16 {
				return &StructureError{Offset: off, Why: fmt.Sprintf("box %q has 64-bit size %d", typ, size)}
			}
		default:
			if size < boxHeader {
				return &StructureError{Offset: off, Why: fmt.Sprintf("box %q has size %d, smaller than its header", typ, size)}
			}
		}

		off += size
		if off > int64(len(buf)) {
			return nil // 多半是 mdat，太大没进探测窗口：正常，不再往下验
		}
	}
	return nil
}

// boxTypeLooksSane 判断这四个字节像不像一个 box 类型码：全是可打印 ASCII。
// 真实的类型码都是 "ftyp"、"moov"、"mdat"、"free" 这种；拼错了多半是一串随机字节。
func boxTypeLooksSane(typ string) bool {
	for i := 0; i < len(typ); i++ {
		if typ[i] < 0x20 || typ[i] > 0x7e {
			return false
		}
	}
	return true
}
