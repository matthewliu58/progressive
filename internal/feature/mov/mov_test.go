package mov

// mov.go 的测试：MOV 自己的头识别、品牌、编码判断。
//
// 地图（index.go）和结构校验（verify.go）各在同级文件里测。共用工具放本文件。
//
// 原则：跟 MP4 重了的地方不重复测（mp4 包那边测过），这里只测 MOV 特有的差别。

import (
	"encoding/binary"
	"testing"

	"progrescarve/internal/feature/mp4"
)

// ---- 共用工具 ----

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u32s(vals ...uint32) []byte {
	out := []byte(nil)
	for _, v := range vals {
		out = append(out, u32(v)...)
	}
	return out
}

func cat(parts ...[]byte) []byte {
	out := []byte(nil)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// buildAtom 搭一个只有头的 atom（payload 留空），长度用 size。
func buildAtom(typ string, size int) []byte {
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b, uint32(size))
	copy(b[4:8], typ)
	return b
}

func box(typ string, body []byte) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:8], typ)
	return append(out, body...)
}

// fullBox 搭一个 full box：类型码后面先跟 4 字节的 version + flags。
func fullBox(typ string, body []byte) []byte {
	return box(typ, append(make([]byte, 4), body...))
}

// buildMoov 搭一个最小可用 moov：一条 vide 轨，一个 chunk 装两个采样。
// stbl 那几张表跟 MP4 完全一样，所以这里只管把它搭出来能解析就行。
func buildMoov(codec string) []byte {
	avcC := box("avcC", []byte{1, 100, 0, 31, 0xff}) // lengthSizeMinusOne = 3 → 长度前缀 4 字节
	stsd := fullBox("stsd", cat(u32(1), box(codec, avcC)))
	stsz := fullBox("stsz", cat(u32(0), u32(2), u32s(4096, 4096)))
	stsc := fullBox("stsc", cat(u32(1), u32(1), u32(2), u32(1))) // first_chunk=1, spc=2, sdi=1
	stco := fullBox("stco", cat(u32(1), u32(0)))                 // 1 个 chunk，偏移 0
	stbl := box("stbl", cat(stsd, stsz, stsc, stco))
	hdlr := fullBox("hdlr", cat(u32(0), []byte("vide")))
	mdia := box("mdia", cat(hdlr, box("minf", stbl)))
	return box("moov", box("trak", mdia))
}

// indexWithCodec 造一张只有两个考点的地图，编码由参数给。
func indexWithCodec(codec string) Index {
	return Index{Index: mp4.Index{
		Samples: []mp4.Sample{{Offset: 0, Size: 4096}, {Offset: 4096, Size: 4096}},
		Codec:   codec,
		NALSize: 4,
	}}
}

// nalData 造一簇采样起点上真的有 NAL 头的数据。
func nalData() []byte {
	d := make([]byte, 8192)
	for _, off := range []int{0, 4096} {
		binary.BigEndian.PutUint32(d[off:], 3000)
		d[off+4] = 0x41 // NAL 类型 1
	}
	return d
}

// ---- mov.go ----

// TestIsMOVHeaderAcceptsLayoutsWithoutFTYP 传统 QuickTime 没有 ftyp，文件直接从
// moov 开头 —— mp4 那条「必须是 ftyp」会把这类好文件判成不是文件头。
func TestIsMOVHeaderAcceptsLayoutsWithoutFTYP(t *testing.T) {
	if !IsMOVHeader(buildAtom("ftyp", 32)) {
		t.Error("ftyp 开头应当认成 MOV 头")
	}
	if !IsMOVHeader(buildAtom("moov", 4096)) {
		t.Error("moov 开头（传统 QuickTime，无 ftyp）也应当认成 MOV 头")
	}
}

func TestIsMOVHeaderRejectsJunk(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"全 0", make([]byte, 64)},
		{"未知类型开头", buildAtom("mdat", 64)},
		{"数据太短", []byte{0, 0, 0, 0x20, 'f', 't', 'y'}},
		{"长度比头还小", []byte{0, 0, 0, 0x04, 'm', 'o', 'o', 'v', 0, 0, 0, 0}},
	} {
		if IsMOVHeader(tc.data) {
			t.Errorf("%s 不应当认成 MOV 头", tc.name)
		}
	}
}

// TestBrand 品牌只用来说明是哪一族，不参与能不能考的判断。
func TestBrand(t *testing.T) {
	for _, tc := range []struct {
		data string
		want string
	}{
		{"ftypqt  ", "mov"},
		{"ftypisom", "mp4"},
		{"ftypmp42", "mp4"},
		{"random", ""},
	} {
		if got := Brand([]byte(tc.data)); got != tc.want {
			t.Errorf("Brand(%q) = %q, want %q", tc.data, got, tc.want)
		}
	}
}

// TestCodecTagFindsMovOnlyCodecs mp4.CodecTag 认不出的 ProRes 要在这里认出来 ——
// 认出了才能在日志里说清「这份文件为什么用不了地图」。
func TestCodecTagFindsMovOnlyCodecs(t *testing.T) {
	for _, want := range []string{"apcn", "AVdn", "svq3"} {
		if got := CodecTag([]byte("junk" + want + "junk")); got == "" {
			t.Errorf("CodecTag 没认出 %q", want)
		}
	}
	if got := CodecTag(make([]byte, 1024)); got != "" {
		t.Errorf("CodecTag(全 0) = %q, want 空串", got)
	}
}

// TestCanNALExamIsWhitelist 白名单的意义：不认识的编码一律不考。
// 漏认只是不用地图，误认会把正确的簇全否掉。
func TestCanNALExamIsWhitelist(t *testing.T) {
	for _, codec := range []string{"apcn", "AVdn", "", "jpeg", "some-new-codec"} {
		if CanNALExam(codec) {
			t.Errorf("CanNALExam(%q) = true, want false", codec)
		}
	}
	for _, codec := range []string{"avc1", "hvc1"} {
		if !CanNALExam(codec) {
			t.Errorf("CanNALExam(%q) = false, want true", codec)
		}
	}
}
