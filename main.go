package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// appConfig 是命令行之外可配置的项：待恢复源、恢复输出挂载点、扫描上限。
type appConfig struct {
	// Source 是待恢复的源：裸设备（如 /dev/rdisk5s1）或 dd 出来的镜像文件。
	Source string `json:"source"`
	// CardRoot 是恢复输出所在挂载点：必须真是挂载点（见 isMountPoint）。
	// 数据收进它下面的自建子目录 recovered，避免 O_TRUNC 误截断卡上原有文件。
	CardRoot string `json:"card_root"`
	// ScanLimitGB 是期望扫描量（GB）。内部封顶 1TB —— 设得再大也只扫到 1T。
	// 删除文件通常集中在低簇位，你这批都在 300~8300 簇，8 就足够覆盖。
	ScanLimitGB int `json:"scan_limit_gb"`
}

// defaultConfig 是找不到配置文件时的兜底值。
var defaultConfig = appConfig{
	Source:      "/dev/rdisk5s1",
	CardRoot:    "/Volumes/test1",
	ScanLimitGB: 8,
}

// loadConfig 读 config.json。文件不存在或解析失败都返回默认（不致命），
// 调用方会记日志，仍能用旧行为跑起来。
//
// JSON 里只写了部分字段时，缺的字段补回默认 —— 否则空字符串 / 0 会悄无声息地
// 把源 / 输出指向错误位置（比如 CardRoot 为空时 isMountPoint 直接拦掉，但报错信息看不出根因）。
func loadConfig(path string) (appConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultConfig, err
	}
	var cfg appConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultConfig, err
	}
	if cfg.Source == "" {
		cfg.Source = defaultConfig.Source
	}
	if cfg.CardRoot == "" {
		cfg.CardRoot = defaultConfig.CardRoot
	}
	if cfg.ScanLimitGB <= 0 {
		cfg.ScanLimitGB = defaultConfig.ScanLimitGB
	}
	return cfg, nil
}

func main() {

	fmt.Println("=== MAIN START1 ===")

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

	fmt.Println("=== MAIN START2 ===")

	logger := slog.New(&SourceHandler{handler: slog.NewMultiHandler(handlers...)})
	slog.SetDefault(logger)

	logger.Info("finit log success", slog.String("pre", pre))

	// 源卡、输出挂载点、扫描上限都从 config.json 读进来，不写死在代码里。
	cfg, err := loadConfig("config.json")
	if err != nil {
		// 没配置文件不致命：退回默认（见 defaultConfig），但记一笔，免得事后不知道
		// 实际用的是哪个源 / 哪个输出 / 扫多少。
		logger.Warn("config file not loaded, using defaults",
			slog.String("pre", pre), slog.String("path", "config.json"), slog.Any("err", err))
	} else {
		logger.Info("config loaded", slog.String("pre", pre),
			slog.String("source", cfg.Source), slog.String("card_root", cfg.CardRoot),
			slog.Int("scan_limit_gb", cfg.ScanLimitGB))
	}

	// 源卡用裸设备读，或 dd 出来的镜像文件。输出挂载点不行：设备节点不是目录。
	path := cfg.Source

	// 恢复数据收进自建子目录：写文件用 O_TRUNC，跟卡上原有文件重名会静默截断重写。
	// 隔一层之后这个风险只落在我们自己产出的东西上，卡上原有数据一个都不会被碰。
	cardRoot := cfg.CardRoot

	// 扫描上限由 GB 换算成字节，传给 Engine；Scan 内部会再封顶到 1TB。
	scanLimitBytes := int64(cfg.ScanLimitGB) << 30
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

	// Engine 的返回值不能丢：识别不出文件系统、设备打不开这些失败只在日志里有，
	// 返回值一丢进程照样往下走，最后退出码还是 0，脚本和 CI 都看不出失败过。
	if err := recovery.Engine(path, outRoot, scanLimitBytes, pre, logger); err != nil {
		logger.Error("recovery engine failed", slog.String("pre", pre), slog.Any("err", err))
		os.Exit(1)
	}

	router := gin.Default()
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, "success")
	})
	logger.Info("api port started", slog.String("pre", pre), slog.String("port", ":8080"))
	if err := router.Run(":8080"); err != nil {
		logger.Error("service start failed", slog.String("pre", pre), slog.Any("err", err))
	}

}
