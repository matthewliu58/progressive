package mov

// 本文件校验 MOV 文件开头的 atom 结构说不说得通。
//
// 为什么不直接用 mp4.CheckStructure：那条校验要求「第一个盒子必须是 ftyp」——
// 这对 MP4 是对的（ISO 规范强制），对 MOV 却是误杀。ftyp 是 QTFF 后来为对齐
// ISO 才补上的，传统 QuickTime 布局压根没有它，文件直接从 moov 开头。
// 拿 MP4 那条去卡，这类好文件会被判成结构坏的。

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// structureProbeSize 是结构校验最多看多少字节。
//
// 顶层 atom 头都在文件最前面，没必要把后面几百 MB 的媒体数据也读一遍 ——
// 校验要便宜，否则没人敢开着它跑全盘恢复。
const structureProbeSize = 1 << 20

// StructureError 表示文件开头这套 atom 结构说不通。
type StructureError struct {
	Offset int64  // 在哪个字节上发现的
	Why    string // 具体哪条判据没过
}

func (e *StructureError) Error() string {
	return fmt.Sprintf("mov atom structure invalid at offset %d: %s", e.Offset, e.Why)
}

// CheckStructure 检查 r 的开头是不是一套说得通的 MOV / QTFF 顶层 atom 结构。
//
// 跟 mp4.CheckStructure 的差别只有「开头允许什么」这一条：
//   - ftyp：带盒型标记的布局（现代 MOV 常见，品牌是 qt  ）；
//   - moov：传统 QuickTime，没有 ftyp，第一个 atom 就是 moov。
//
// 其余判据两家一致（长度不小于头长、类型码四个可打印字符），因为 atom 的排布
// 规则本来就是同一套。MOV 多出来的那些顶层类型（wide / free / skip / pnot /
// PICT…）不需要单列清单 —— 只要类型码可打印就过得去，单列反而会漏掉没见过的。
//
// 能验到什么程度跟 mp4 那边一样要讲清楚：mdat 内部没有同步结构，中间被换掉几簇
// 是查不出来的。它能挡住的是「第一簇就拼错」和「拼进来的是别人的数据」这两类。
func CheckStructure(r io.Reader) error {
	buf := make([]byte, structureProbeSize)
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	if n < boxHeader {
		return &StructureError{Offset: 0, Why: "file shorter than one atom header"}
	}
	buf = buf[:n]

	if first := string(buf[4:8]); first != "ftyp" && first != "moov" {
		return &StructureError{Offset: 0, Why: fmt.Sprintf("first atom is %q, want \"ftyp\" or \"moov\"", first)}
	}

	off := int64(0)
	for off+boxHeader <= int64(len(buf)) {
		typ := string(buf[off+4 : off+8])
		if !atomTypeLooksSane(typ) {
			return &StructureError{Offset: off, Why: fmt.Sprintf("atom type %q is not four printable chars", typ)}
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
				return &StructureError{Offset: off, Why: fmt.Sprintf("atom %q has 64-bit size %d", typ, size)}
			}
		default:
			if size < boxHeader {
				return &StructureError{Offset: off, Why: fmt.Sprintf("atom %q has size %d, smaller than its header", typ, size)}
			}
		}

		off += size
		if off > int64(len(buf)) {
			return nil // 多半是 mdat，太大没进探测窗口：正常，不再往下验
		}
	}
	return nil
}

// atomTypeLooksSane 判断这四个字节像不像一个 atom 类型码：全是可打印 ASCII。
//
// 真实的类型码都是 "ftyp"、"moov"、"mdat"、"wide" 这种；拼错了多半是一串随机字节。
func atomTypeLooksSane(typ string) bool {
	for i := 0; i < len(typ); i++ {
		if typ[i] < 0x20 || typ[i] > 0x7e {
			return false
		}
	}
	return true
}
