package mov

// 本文件是一份 MOV 的 moov 地图。
//
// 先说清楚它为什么不自己解析一遍：stbl 那四张表（stco/co64 + stsc + stsz + stss）
// 在 MOV 和 MP4 里是同一份规范、连名字都一样，所以「由 moov 算出每个采样在文件
// 第几字节」这件事，MOV 不需要任何自己的代码 —— 直接用 mp4.ParseIndex。
// 在这里再抄一遍，只会让两边将来改出分歧。
//
// 本文件补的是 MOV 真正要回答的那件事：**这份地图能不能拿来考试**。
// 详见 Exam。

import (
	"progrescarve/internal/feature/mp4"
)

// Index 是一份 MOV 的 moov 地图。
//
// 内嵌 mp4.Index：采样偏移、编码、NAL 长度前缀这些是两家共用的，直接继承，
//
//	Samples / Codec 的用法跟 mp4 完全一致。
//
// Brand 是本包多带的：MOV 还想知道这份文件是哪一族（"mov" / "mp4"），
//
//	因为它决定了「能不能没有 ftyp」这类判断该怎么下。
type Index struct {
	mp4.Index
	Brand string
}

// ParseIndex 从一段完整的 moov（含 atom 头）解析出地图。
//
// 复用 mp4.ParseIndex：stbl 是同一份规范。这里只多做一件事 ——
// 顺手认一下编码臼在哪一类，好在日志里说清「为什么这份文件用不了地图」。
func ParseIndex(moov []byte, brand string) (Index, error) {
	sub, err := mp4.ParseIndex(moov)
	if err != nil {
		return Index{}, err
	}
	return Index{Index: sub, Brand: brand}, nil
}

// CanExam 表示这份地图能不能用来考试：既要有考点，编码也得是 NAL 封装的。
//
// 两个条件缺一不可 —— 见 CanNALExam 里说的，编码不对的时候考一道错一道，
// 那不是「考不过」，是这把尺子根本量不了这个东西。
func (idx Index) CanExam() bool { return idx.Usable() && idx.CanNALExam() }

// Exam 拿这份地图考试一簇数据：假设它覆盖文件偏移 [fileOff, fileOff+len(data))。
//
// 地图上落在这段的每个采样起点都是一个考点：真实文件在这些位置必然是一个 NAL
// 的开始（长度前缀 + NAL 头）。返回考点数、通过数，以及「这考试算不算数」。
//
// applicable 为 false 表示这份文件的编码不能按 NAL 考（Apple ProRes、DNxHD 这些
// MOV 里常见的）：这时 checked / passed 都是 0，意思**不是「没通过」**，
// 而是「这张地图给不出判据」。调用方必须退回别的办法，不能当成否定的证据 ——
// 把它当否决，等于把正确的簇全扔了。
func (idx Index) Exam(data []byte, fileOff uint64) (checked, passed int, applicable bool) {
	if !idx.CanExam() {
		return 0, 0, false
	}
	checked, passed = idx.Match(data, fileOff)
	return checked, passed, true
}
