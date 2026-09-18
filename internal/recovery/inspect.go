package recovery

import (
	"fmt"
	"log/slog"

	"progrescarve/internal/feature"
	"progrescarve/internal/feature/jpeg"

	fsinit "progrescarve/internal/fs/finit"
)

// 这个文件的活只有一件：把某一簇的原始字节和判定依据打到日志里，用来回答
// 「这一簇装的到底是什么、为什么没判成某种格式」。
//
// 为什么需要它：「扫到了但没命中」在日志里等于没有痕迹 —— 命中列表里看不到它，
// 就分不清是「压根没扫到」还是「扫了但判不出来」。所以判不出来的簇也得留一条，
// 而且判据要一起摊开：只知道结论是 KindNone，没法知道该调哪个阈值。

const (
	// inspectHeadBytes 是打出前多少字节。16 字节够看清 box 头（8 字节）和 NAL 的
	// 长度前缀（4 字节）+ NAL 头；再多就是媒体数据本身，读不出什么名堂。
	inspectHeadBytes = 16

	// inspectSpan 是从第一簇往后连看多少簇。文件后续簇就在附近（实测间隔 1~3 簇），
	// 顺序看一小段就能覆盖链上的大部分簇。
	inspectSpan = 12
)

// inspectCluster 把一簇的开头字节和各格式的判据打成一条 Debug 日志。
func inspectCluster(logger *slog.Logger, pre, path string, cid uint32, data []byte) {
	if logger == nil {
		return
	}
	h := feature.Classify(cid, data)

	attrs := []any{
		slog.String("pre", pre),
		slog.String("entry", path),
		slog.Uint64("cluster", uint64(cid)),
		slog.String("kind", h.Kind.String()),
		slog.String("head", hexHead(data, inspectHeadBytes)),
	}

	// 各格式的判据一起摊开：判错了要能看出是哪一条卡住的。
	if m := h.MP4; m != nil {
		attrs = append(attrs,
			slog.Int("mp4_nal_frames", m.NALFrames),
			slog.Int("mp4_nal_run", m.NALRunMax),
			slog.Float64("mp4_nal_density", m.NALDensity),
			slog.Int("mp4_ftyp", m.FTYPOffset),
			slog.Int("mp4_moov", m.MOOVOffset),
			slog.Int("mp4_mdat", m.MDATOffset),
			slog.Int("mp4_len_size", m.LengthSize),
			slog.Bool("mp4_hevc", m.HEVC))
	}
	if j := h.JPEG; j != nil {
		attrs = append(attrs,
			slog.Int("jpeg_ff", j.FFCount),
			slog.Int("jpeg_stuffed", j.StuffedCount),
			slog.Float64("jpeg_ratio", jpeg.StuffedRatio(*j)),
			slog.Int("jpeg_soi", j.SOIOffset),
			slog.Int("jpeg_eoi", j.EOIOffset))
	}

	logger.Debug("cluster inspect", attrs...)
}

// hexHead 把开头 n 个字节写成十六进制。
func hexHead(data []byte, n int) string {
	if len(data) < n {
		n = len(data)
	}
	return fmt.Sprintf("%x", data[:n])
}

// inspectEntrySpan 从一个条目的第一簇开始往后连看若干簇，逐簇打一条。
//
// 只给「猜得出格式」的条目用（现在只有 MP4）：连该是什么都不知道，打了也对不上号。
func inspectEntrySpan(parser fsinit.FileSystemParser, logger *slog.Logger,
	pre, path string, first uint32) {
	if logger == nil {
		return
	}
	for i := range inspectSpan {
		cid := first + uint32(i)
		data, err := parser.ReadCluster(cid)
		if err != nil {
			continue
		}
		inspectCluster(logger, pre, path, cid, data)
	}
}
