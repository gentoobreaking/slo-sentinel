package watch

// watch_test.go（T028）：共用目錄 watcher 的基本行為。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDirNotifiesOnChange(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fired := make(chan struct{}, 4)
	stop, err := Dir(ctx, dir, func() { fired <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)

	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("onChange not fired within 3s of file create")
	}

	// 非 yaml 檔不觸發
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
		t.Fatal(".txt change must not fire")
	case <-time.After(800 * time.Millisecond):
	}
}

func TestDirStopPreventsFurtherCallbacks(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fired := make(chan struct{}, 4)
	stop, err := Dir(ctx, dir, func() { fired <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
	}
	stop() // 之後不得再觸發
	time.Sleep(700 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("x: 2\n"), 0o600)
	select {
	case <-fired:
		t.Fatal("callback fired after stop")
	default:
	}
}
