package png

import (
	"fmt"
	"io"

	stdlib "image/png"
)

// decodeMaxBytes 是整份解码的体积上限。
//
// 解一张图是毫秒级，但目录项被写坏时 DataLength 可能是天文数字 —— 那种情况不该
// 真的去解。超出上限就放弃校验（返回 nil），不判失败：没验过不等于验不过。
const decodeMaxBytes = 64 << 20

// DecodeError 表示这份数据解不出来：不是「解码器不认识」，是数据本身断了或错了。
type DecodeError struct {
	Err error // 解码器给的原因（unexpected EOF、invalid 之类）
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("png decode failed: %v", e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// CheckDecode 把 r 当一份完整的 PNG 解一遍，解不出来就报错。
//
// 为什么要真解一遍：猜出来的簇到底接对没有，只有解码器能回答 —— 它能一路解到 IEND，
// 说明字节流是通的，而且顺带把每个 chunk 的 CRC 和 IDAT 里的 zlib 流都验过了。
// 前面那些判据（chunk 走通、相邻打分、簇边界咬合）全都只能否决，证明不了「接对了」。
//
// size 是这份数据的字节数，用来挡住上面说的「DataLength 被写坏」的情况。
//
// 和 jpeg 不同，标准库 image/png 支持所有标准颜色类型/位深，不存在「解码器不支持
// 这个特性」这类要放行的情况，所以这里不区分 UnsupportedError —— 解不过就是解不过。
func CheckDecode(r io.Reader, size int64) error {
	if size > decodeMaxBytes {
		return nil
	}
	if _, err := stdlib.Decode(r); err != nil {
		return &DecodeError{Err: err}
	}
	return nil
}
