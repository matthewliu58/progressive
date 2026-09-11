package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"progrescarve/internal/recovery"
	"runtime"
	"syscall"

	"github.com/gin-gonic/gin"
)

type SourceHandler struct {
	handler slog.Handler
}

func (h *SourceHandler) Handle(ctx context.Context, r slog.Record) error {
	fs := runtime.CallersFrames([]uintptr{r.PC})
	frame, _ := fs.Next()
	fileName := filepath.Base(frame.File)
	r.AddAttrs(slog.String("file", fileName),
		slog.Int("line", frame.Line), slog.String("func", frame.Func.Name()))
	return h.handler.Handle(ctx, r)
}

func (h *SourceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *SourceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &SourceHandler{handler: h.handler.WithAttrs(attrs)}
}

func (h *SourceHandler) WithGroup(name string) slog.Handler {
	return &SourceHandler{handler: h.handler.WithGroup(name)}
}

// isMountPoint 判断 p 是不是已挂载的文件系统：挂载点的 st_dev 与父目录不同。
// 只看 os.Stat 存不存在不够 —— /Volumes/<卡名> 可能只是个普通目录（卡没挂时被误建过），
// 那时按路径往里写，写的是内置盘。
func isMountPoint(p string) bool {
	clean := filepath.Clean(p)
	if clean == string(filepath.Separator) {
		return true // 根目录的父目录就是它自己，比 st_dev 没有意义
	}
	st, err := os.Stat(clean)
	if err != nil {
		return false
	}
	parent, err := os.Stat(filepath.Dir(clean))
	if err != nil {
		return false
	}
	sd, ok1 := st.Sys().(*syscall.Stat_t)
	pd, ok2 := parent.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && sd.Dev != pd.Dev
}

func main() {
	pre := "progrescarve"

	// 终端用文本格式给人读，文件留 JSON + 源码位置方便事后翻。
	// 只挂文件 handler 的话，跑一次在终端上什么都看不见，看着就像没报错。
	handlers := []slog.Handler{
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}),
	}

	logDir := filepath.Join(".", "log")
	if err := os.MkdirAll(logDir, os.ModePerm); err != nil {
		slog.Error("create log dir failed", slog.Any("err", err), slog.String("path", logDir))
	}
	logFilePath := filepath.Join(logDir, "app.log")
	if logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666); err != nil {
		slog.Error("open log file failed", slog.Any("err", err), slog.String("path", logFilePath))
	} else {
		handlers = append(handlers, slog.NewJSONHandler(logFile, &slog.HandlerOptions{
			Level:     slog.LevelDebug,
			AddSource: true,
		}))
	}

	logger := slog.New(&SourceHandler{handler: slog.NewMultiHandler(handlers...)})
	slog.SetDefault(logger)

	logger.Info("finit log success", slog.String("pre", pre))

	// 源卡用裸设备读。目标卡不行：/dev/rdisk4s1 是设备节点不是目录，只能走挂载点。
	path := "/dev/rdisk5s1" // 旧卡 KOCARDGO（1TB）：待恢复的源

	// 恢复数据收进自建子目录：写文件用 O_TRUNC，跟卡上原有文件重名会静默截断重写。
	// 隔一层之后这个风险只落在我们自己产出的东西上，卡上原有数据一个都不会被碰。
	cardRoot := "/Volumes/test1"
	outRoot := filepath.Join(cardRoot, "recovered")

	// 卡没挂上必须拦住：MkdirAll 会欣然在内置盘上建出 /Volumes/test1，
	// 数据就全写到 Mac 自己的系统盘上了。
	if !isMountPoint(cardRoot) {
		logger.Error("card root is not a mount point, mount the card first",
			slog.String("pre", pre), slog.String("card_root", cardRoot))
		os.Exit(1)
	}

	// 开工前先建出来：Engine 前半段要扫完整张卡（1TB 得几分钟），
	// 等第一条文件落地才发现卡只读或满了就白扫了。
	if err := os.MkdirAll(outRoot, 0o755); err != nil {
		logger.Error("create output root failed", slog.String("pre", pre),
			slog.String("out", outRoot), slog.Any("err", err))
		os.Exit(1)
	}

	recovery.Engine(path, outRoot, pre, logger)

	router := gin.Default()
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, "success")
	})

	var err error
	logger.Info("api port started", slog.String("pre", pre), slog.String("port", ":8080"))
	if err = router.Run(":8080"); err != nil {
		logger.Error("service start failed", slog.String("pre", pre), slog.Any("err", err))
		return
	}
}
