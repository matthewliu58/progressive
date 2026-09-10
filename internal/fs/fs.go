package fs

import (
	"log/slog"
	"os"
	"progrescarve/internal/fs/exfat"
	fsinit "progrescarve/internal/fs/finit"
)

const (
	EXFA = "EXFA"
)

func DetectFileSystem(f *os.File, logger *slog.Logger, pre string) (fsinit.FileSystemParser, error) {
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
