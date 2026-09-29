package mov

// index.go 的测试：MOV 的 moov 地图。
//
// 地图本身（stbl 那几张表）MOV 和 MP4 是同一份规范、同一份代码，mp4 包测过；
// 这里要验证的是两件事：
//  1. 地图能建起来 —— 「MOV 也走这条路」不是嘴上说说，得真能解析出采样偏移；
//  2. 编码不能考的时候不许交出「没通过」的结论 —— 那会把正确的簇整片否掉。

import (
	"encoding/binary"
	"testing"
)

// TestParseIndexBuildsMapForMOV MOV 的 moov 能解析出地图：stbl 跟 MP4 同一份规范，
// 所以这里预期跟 mp4 手工验算的结果一致（两个采样：0 和 4096）。
func TestParseIndexBuildsMapForMOV(t *testing.T) {
	idx, err := ParseIndex(buildMoov("avc1"), "mov")
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	if idx.Brand != "mov" {
		t.Errorf("Brand = %q, want mov", idx.Brand)
	}
	if len(idx.Samples) != 2 {
		t.Fatalf("采样数 = %d, want 2", len(idx.Samples))
	}
	if idx.Samples[0].Offset != 0 || idx.Samples[1].Offset != 4096 {
		t.Errorf("采样偏移 = %d / %d, want 0 / 4096",
			idx.Samples[0].Offset, idx.Samples[1].Offset)
	}
	if !idx.CanExam() {
		t.Error("avc1 的地图应当能考：有考点且是 NAL 封装")
	}
}

// TestExamRefusesNonNALCodecs 苹果自己的编码不能按 NAL 考。
//
// 关键不是「考不过」，而是「根本不给判据」：applicable 为 false，checked 必须是 0。
// 调用方看到 checked == 0 才知道该退回老办法，而不是以为这一簇被否定了。
func TestExamRefusesNonNALCodecs(t *testing.T) {
	for _, codec := range []string{"apcn", "apch", "ap4h", "AVdn", "rle ", "jpeg", "svq3", ""} {
		checked, passed, applicable := indexWithCodec(codec).Exam(nalData(), 0)
		if applicable {
			t.Errorf("codec %q: applicable = true, want false（不是 NAL 封装的不能考）", codec)
		}
		if checked != 0 || passed != 0 {
			t.Errorf("codec %q: checked=%d passed=%d, want 0 0（给不出判据时不许输出否定结论）",
				codec, checked, passed)
		}
	}
}

// TestExamAcceptsNALCodecs H.264 / HEVC 的 MOV 跟 MP4 一样考：
// 地图通用（stbl 是同一份规范），考试也通用。
func TestExamAcceptsNALCodecs(t *testing.T) {
	for _, codec := range []string{"avc1", "avc3", "hvc1", "hev1"} {
		checked, passed, applicable := indexWithCodec(codec).Exam(nalData(), 0)
		if !applicable {
			t.Fatalf("codec %q: applicable = false, want true", codec)
		}
		if checked != 2 || passed != 2 {
			t.Errorf("codec %q: checked=%d passed=%d, want 2 2", codec, checked, passed)
		}
	}
}

// TestExamStillRejectsForeignDataForNALCodecs 能考的前提下不放水 ——
// mp4 那边验证过的分辨力，在 MOV 上得保持一致。
func TestExamStillRejectsForeignDataForNALCodecs(t *testing.T) {
	// 采样起点上放一个离谱的长度前缀（超过单个 NAL 的上限）：不是 NAL。
	foreign := make([]byte, 8192)
	binary.BigEndian.PutUint32(foreign[0:], 1<<30)

	checked, passed, applicable := indexWithCodec("avc1").Exam(foreign, 0)
	if !applicable || checked != 2 {
		t.Fatalf("applicable=%v checked=%d, want true 2", applicable, checked)
	}
	if passed != 0 {
		t.Errorf("passed = %d, want 0 —— 这一簇在采样起点上不是 NAL", passed)
	}
}

// TestExamNoSamplesIsNotStructured 没有考点的地图也不能考（Usable 为假）。
func TestExamNoSamplesIsNotStructured(t *testing.T) {
	empty := indexWithCodec("avc1")
	empty.Samples = nil // 地图上没有考点：任何结论都不能给
	if empty.CanExam() {
		t.Error("没有采样时不应当能考")
	}
	if checked, passed, applicable := empty.Exam(nalData(), 0); checked != 0 || passed != 0 || applicable {
		t.Errorf("空地图 Exam = (%d, %d, %v), want (0, 0, false)", checked, passed, applicable)
	}
}
