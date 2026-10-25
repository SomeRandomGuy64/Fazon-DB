package main

import "testing"

func TestSyncSafeName(t *testing.T) {
	long := "【ダイアン】" + "とても長いタイトルとても長いタイトルとても長いタイトルとても長いタイトルとても長いタイトル#ダイアン_0.01.02.345-0.01.04.567.mp3"
	short, err := syncSafeName(long)
	if err != nil || len(short) > 120 {
		t.Fatalf("%q %v (%d B)", short, err, len(short))
	}
	if !isSyncSafeName("abc_1-2.mp3") || mustShort("abc_1-2.mp3") != "abc_1-2.mp3" {
		t.Fatal("short name changed")
	}
	f := `[sound:` + long + `]`
	if got := renameMediaRefs(f, long, short); got != "[sound:"+short+"]" {
		t.Fatal(got)
	}
	img := `<img src="` + "a%20b.jpg" + `">`
	if got := renameMediaRefs(img, "a b.jpg", "c.jpg"); got != `<img src="c.jpg">` {
		t.Fatal(got)
	}
	t.Log(short)
}

func mustShort(s string) string { r, _ := syncSafeName(s); return r }
