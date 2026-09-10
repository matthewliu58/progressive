package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"progrescarve/internal/recovery"
	"runtime"

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

func main() {
	logDir := filepath.Join(".", "log")
	_ = os.MkdirAll(logDir, os.ModePerm)
	logFilePath := filepath.Join(logDir, "app.log")
	logFile, _ := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)

	baseHandler := slog.NewTextHandler(logFile, &slog.HandlerOptions{
		Level:     slog.LevelDebug,
		AddSource: true,
	})
	logger := slog.New(&SourceHandler{handler: baseHandler})
	slog.SetDefault(logger)

	pre := "progrescarve"
	logger.Info("finit log success", slog.String("pre", pre))

	path := "/dev/rdisk5s1"
	recovery.Engine(path, pre, logger)

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
