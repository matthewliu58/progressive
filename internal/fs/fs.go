package fs

import (
	"log/slog"
	"os"
	"progrescarve/internal/fs/exfat"
	fsinit "progrescarve/internal/fs/finit"
)

// EXFA 是 exFAT 卷的标识串，落在引导扇区偏移 3 的位置。
const (
	EXFA = "EXFA"
)

// DetectFileSystem 认出这个卷是什么文件系统。
//
// 认不出来时返回 (nil, nil) 而不是错误：调用方必须判一次 nil 再用，
// 直接拿返回值去调方法会空指针 panic。
func DetectFileSystem(f *os.File, logger *slog.Logger, pre string) (fsinit.FileSystemParser, error) {
	// 只读第一个扇区：文件系统标识就在引导扇区开头，不需要多读。
	bootBuf := make([]byte, 512)
	_, err := f.ReadAt(bootBuf, 0)
	if err != nil {
		logger.Warn("Failed to detect boot buffer", slog.String("pre", pre), "error", err, "file", f.Name())
		return nil, err
	}
	sig := string(bootBuf[3:7])
	if sig == EXFA {
		return exfat.NewExFatParser(pre, logger), nil
	}
	return nil, nil
}
