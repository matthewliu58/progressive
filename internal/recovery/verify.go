package recovery

import (
	"fmt"
	"os"

	"progrescarve/internal/feature"
	"progrescarve/internal/feature/jpeg"
	"progrescarve/internal/feature/mp4"
)

// VerifyError 表示拼出来的这份文件过不了校验：猜的那几簇大概接错了。
//
// 这是「组合之后」才有的一道关。前面所有判据（内容特征、相邻打分、簇号距离）都只能
// 说「没发现不对」，只有解码器能说「这份是通的」。所以猜出来的簇拼完之后必须过这一关，
// 过不了就把猜的部分扔掉 —— 留着就是写一份坏数据，比少给几簇难发现得多：
// 文件体积合法、名字也对，只有打开时才看出是坏的。
type VerifyError struct {
	Path   string       // 条目路径
	Format feature.Kind // 按什么格式验的
	Carved int          // 猜了几簇（这些簇随后要释放，见 RestoreAllDeleted）
	Err    error        // 解码器/结构校验给的原因
}

func (e *VerifyError) Error() string {
	return fmt.Sprintf("%q does not decode as %s after appending %d guessed clusters: %v",
		e.Path, e.Format, e.Carved, e.Err)
}

func (e *VerifyError) Unwrap() error { return e.Err }

// verifyOutput 校验刚写到 dst 的这份文件能不能被它自己格式的解码器接受。
//
// 返回 nil 表示通过，或者**没法验**（格式不认识）—— 没验过不等于验不过，不能因此判失败。
// 具体判据在各个格式包里：JPEG 真解一遍，MP4 走一遍 box 结构。recovery 只负责分发。
func verifyOutput(dst string, format feature.Kind) error {
	f, err := os.Open(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	switch format {
	case feature.KindJPEGHeader, feature.KindJPEGPayload:
		return jpeg.CheckDecode(f, fi.Size())
	case feature.KindMP4Header, feature.KindMP4Payload:
		return mp4.CheckStructure(f)
	default:
		return nil
	}
}
