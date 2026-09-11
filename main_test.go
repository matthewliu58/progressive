package main

import "testing"

// TestIsMountPoint 盯住「目标卡到底挂没挂」：卡没挂时 /Volumes/<卡名> 只是个普通目录，
// MkdirAll 会把它建在内置盘上，恢复数据全写到 Mac 系统盘，日志看着还一切正常。
func TestIsMountPoint(t *testing.T) {
	if !isMountPoint("/") {
		t.Errorf(`isMountPoint("/") = false, want true`)
	}
	// 临时目录和它的父目录在同一个文件系统上，不是挂载点。
	if isMountPoint(t.TempDir()) {
		t.Errorf("isMountPoint(TempDir) = true, want false")
	}
	// 卡没插时 /Volumes 下不会有同名目录。
	if isMountPoint("/Volumes/progrescarve-not-a-real-card") {
		t.Errorf("不存在的路径不该被判成挂载点")
	}
}
