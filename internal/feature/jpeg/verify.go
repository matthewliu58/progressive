package jpeg

import (
	"errors"
	"fmt"
	"io"

	stdlib "image/jpeg"
)

// decodeMaxBytes 是整份解码的体积上限。
//
// 解一张图是毫秒级，但目录项被写坏时 DataLength 可能是天文数字 —— 那种情况不该
// 真的去解。超出上限就放弃校验（返回 nil），不判失败：没验过不等于验不过。
const decodeMaxBytes = 64 << 20

// DecodeError 表示这份数据解不出来：不是「解码器不认识」，是数据本身断了或错了。
type DecodeError struct {
	Err error // 解码器给的原因（unexpected EOF、corrupt 之类）
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("jpeg decode failed: %v", e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// CheckDecode 把 r 当一份完整的 JPEG 解一遍，解不出来就报错。
//
// 为什么要真解一遍：猜出来的簇到底接对没有，只有解码器能回答 —— 它能一路解到 EOI，
// 说明字节流是通的。前面那些判据（FF 00 比例、相邻打分、簇号距离）全都只能否决，
// 证明不了「接对了」。
//
// size 是这份数据的字节数，用来挡住上面说的「DataLength 被写坏」的情况。
//
// 两类「解不出来」必须分开：
//   - 数据断了/错了（unexpected EOF、invalid、corrupt……）：真失败，返回错误；
//   - 解码器不支持这个特性（算术编码、12 位精度之类）：图本身可能是好的，
//     不能因为标准库不支持就把它判成拼错了 —— 返回 nil。
func CheckDecode(r io.Reader, size int64) error {
	if size > decodeMaxBytes {
		return nil
	}
	if _, err := stdlib.Decode(r); err != nil {
		var unsupported *stdlib.UnsupportedError
		if errors.As(err, &unsupported) {
			return nil
		}
		return &DecodeError{Err: err}
	}
	return nil
}
