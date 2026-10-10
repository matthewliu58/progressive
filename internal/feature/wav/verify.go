package wav

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// structureMaxBytes 是整份结构校验的体积上限。
//
// 验一份 WAV 是毫秒级，但目录项被写坏时 DataLength 可能是天文数字 —— 那种情况不该
// 真的去读。超出上限就放弃校验（返回 nil），不判失败：没验过不等于验不过。
const structureMaxBytes = 64 << 20

// StructureError 表示这份数据走不通 RIFF/WAVE 结构：不是「解码器不认识」，是结构本身错了。
type StructureError struct {
	Offset int64 // 在哪个字节上发现的
	Why    string
}

func (e *StructureError) Error() string {
	return fmt.Sprintf("wav structure invalid at offset %d: %s", e.Offset, e.Why)
}

// CheckStructure 校验 r 的开头是不是一套说得通的 RIFF/WAVE 结构。
//
// 为什么要自己走一遍而不是「真解码」：Go 标准库没有 WAV 解码器，而且雕刻恢复要的只是
// 「这份字节流通不通」—— RIFF 头对得上、"fmt" 字段合法、chunk 一路走得通，就够了。
// 真正「接对了」只有这一关能回答：它能一路走到 data 块，说明字节流在结构上没断。
//
// 和 jpeg/png 的 CheckDecode 不同，这里是结构校验（跟 mp4/mov 的 CheckStructure 同类）：
// WAV 没有像素可解，也没有 CRC 可验，能做的就是这个。签名和 fmt 都在文件最前面，
// 所以只验开头一段就够（见 structureMaxBytes）；截断在 data 块中间是雕刻文件的常态
// （按 DataLength 喂饱即止），不判失败；只有结构本身 incoherent（chunk ID 不可打印、
// fmt 字段非法、走飞）才报错。
func CheckStructure(r io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(r, structureMaxBytes))
	if err != nil {
		return err
	}
	if len(data) < riffHeader {
		return &StructureError{Offset: 0, Why: "file shorter than RIFF header"}
	}
	if !bytes.Equal(data[0:4], riffMagic) || !bytes.Equal(data[8:12], waveMagic) {
		return &StructureError{Offset: 0, Why: "not a RIFF/WAVE file"}
	}

	off := riffHeader
	fmtSeen := false
	for off+chunkHeader <= len(data) {
		id := data[off : off+4]
		if !isValidChunkID(id) {
			return &StructureError{Offset: int64(off), Why: fmt.Sprintf("chunk id %q is not printable ASCII", id)}
		}

		length := binary.LittleEndian.Uint32(data[off+4 : off+8])
		if length > maxChunkLength {
			return &StructureError{Offset: int64(off), Why: fmt.Sprintf("chunk %q has absurd length %d", id, length)}
		}

		end := off + chunkHeader + int(length)
		if end < off { // 溢出防护
			return &StructureError{Offset: int64(off), Why: "chunk length overflow"}
		}
		// RIFF 奇数长度补 1 字节 padding。
		if length%2 == 1 {
			end++
		}

		if string(id) == "fmt " {
			// fmt 块就在文件最前面，structureMaxBytes 足以把它整块读进来；
			// 即便被截断也至少是声明的长度，照常校验。
			avail := end
			if avail > len(data) {
				avail = len(data)
			}
			if err := checkFmt(data[off+chunkHeader : avail]); err != nil {
				return err
			}
			fmtSeen = true
		}

		if end > len(data) {
			// 截断在 chunk 数据中间：雕刻出来的文件常见，允许。
			break
		}
		off = end
	}

	if !fmtSeen {
		return &StructureError{Offset: 0, Why: "no fmt chunk"}
	}
	return nil
}

// checkFmt 校验 "fmt " 块的核心字段是否合法。只看通用字段（格式标签、声道数、采样率、
// 位深），不深究 extensible 的扩展段 —— 这些字段在所有 WAV 变体里位置固定、足以
// 挡掉「拼进来的是别人的数据」这类错误。
func checkFmt(b []byte) error {
	if len(b) < 16 {
		return &StructureError{Offset: 0, Why: "fmt chunk shorter than 16 bytes"}
	}

	format := binary.LittleEndian.Uint16(b[0:2])
	switch format {
	case 0x0001, 0xFFFE, 0x0003: // PCM / extensible / IEEE float
		// 认识的格式标签
	default:
		return &StructureError{Offset: 0, Why: fmt.Sprintf("unsupported wave format tag %#x", format)}
	}

	channels := binary.LittleEndian.Uint16(b[2:4])
	if channels == 0 {
		return &StructureError{Offset: 0, Why: "zero channels"}
	}

	sampleRate := binary.LittleEndian.Uint32(b[4:8])
	if sampleRate == 0 {
		return &StructureError{Offset: 0, Why: "zero sample rate"}
	}

	bits := binary.LittleEndian.Uint16(b[14:16])
	if bits == 0 || bits > 32 {
		return &StructureError{Offset: 0, Why: fmt.Sprintf("bad bits per sample %d", bits)}
	}
	return nil
}
