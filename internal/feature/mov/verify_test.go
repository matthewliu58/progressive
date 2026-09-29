package mov

// verify.go 的测试：结构校验。
//
// 唯一要盯死的是「开头允许什么」—— mp4 要求必须是 ftyp，MOV 允许 ftyp 或 moov。
// 这条判据放宽是为了不误杀老 QuickTime，但也不能松到什么都收。

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestCheckStructureAcceptsFTYPFirst 现代布局：ftyp 开头。
func TestCheckStructureAcceptsFTYPFirst(t *testing.T) {
	buf := cat(
		buildAtom("ftyp", 32),
		buildAtom("moov", 64),
	)
	if err := CheckStructure(bytes.NewReader(buf)); err != nil {
		t.Errorf("ftyp 开头应当通过校验: %v", err)
	}
}

// TestCheckStructureAcceptsMoovFirst 传统 QuickTime 布局：没有 ftyp，直接从 moov
// 开头。这条是 mov 跟 mp4 的校验真正的差别 —— mp4.CheckStructure 会判它失败。
func TestCheckStructureAcceptsMoovFirst(t *testing.T) {
	buf := cat(
		buildAtom("moov", 128),
		buildAtom("mdat", 64),
	)
	if err := CheckStructure(bytes.NewReader(buf)); err != nil {
		t.Errorf("moov 开头（传统 QuickTime）应当通过校验: %v", err)
	}
}

// TestCheckStructureRejectsWrongFirstAtom 开头不是这两种的不收。
func TestCheckStructureRejectsWrongFirstAtom(t *testing.T) {
	buf := buildAtom("mdat", 64)
	err := CheckStructure(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("mdat 开头应当判失败")
	}
	var se *StructureError
	if !asStructureError(err, &se) {
		t.Fatalf("错误类型 = %T, want *StructureError", err)
	}
	if se.Offset != 0 {
		t.Errorf("Offset = %d, want 0", se.Offset)
	}
}

// TestCheckStructureRejectsGarbageTypeCodes 类型码里有不可打印字符 —— 多半是拼进
// 来了别人的随机字节。
func TestCheckStructureRejectsGarbageTypeCodes(t *testing.T) {
	buf := make([]byte, 64)
	binary.BigEndian.PutUint32(buf, 32)
	copy(buf[4:8], "ftyp")
	// 第二个 atom 的类型码放一串非打印字节。
	copy(buf[32:36], []byte{0, 1, 2, 3})
	binary.BigEndian.PutUint32(buf[32:], 32)

	if err := CheckStructure(bytes.NewReader(buf)); err == nil {
		t.Error("类型码不可打印时应当判失败")
	}
}

// TestCheckStructureToleratesShortFile 文件比一个 atom 头还短：那是没内容可验，
// 判据要说清楚（返回错误，不是 panic）。
func TestCheckStructureToleratesShortFile(t *testing.T) {
	if err := CheckStructure(bytes.NewReader([]byte{0, 0})); err == nil {
		t.Error("比一个 atom 头还短应当判失败")
	}
}

// TestCheckStructureToleratesMovOnlyAtoms MOV 特有的顶层 atom（wide / free / skip）
// 不能因为它少见就判坏 —— 它们的类型码是可打印的，照常放行。
func TestCheckStructureToleratesMovOnlyAtoms(t *testing.T) {
	buf := cat(
		buildAtom("ftyp", 32),
		buildAtom("wide", 8),
		buildAtom("free", 16),
		buildAtom("skip", 16),
		buildAtom("mdat", 64),
	)
	if err := CheckStructure(bytes.NewReader(buf)); err != nil {
		t.Errorf("MOV 特有的顶层 atom 应当放行: %v", err)
	}
}

// asStructureError 把 error 拆成 *StructureError，方便检查是哪个字节出的问题。
func asStructureError(err error, target **StructureError) bool {
	se, ok := err.(*StructureError)
	if ok {
		*target = se
	}
	return ok
}
