package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCollectConflicts(t *testing.T) {
	target := t.TempDir()
	stage := t.TempDir()

	writeTestFile(t, filepath.Join(target, "same.txt"), "old")
	writeTestFile(t, filepath.Join(target, "replace-dir", "nested.txt"), "nested")
	writeTestFile(t, filepath.Join(target, "merge", "keep.txt"), "keep")
	writeTestFile(t, filepath.Join(target, "merge", "swap"), "file")
	writeTestFile(t, filepath.Join(target, "folder-to-file", "inside.txt"), "inside")

	writeTestFile(t, filepath.Join(stage, "same.txt"), "new")
	writeTestFile(t, filepath.Join(stage, "replace-dir"), "flat")
	writeTestFile(t, filepath.Join(stage, "merge", "swap", "child.txt"), "child")
	writeTestFile(t, filepath.Join(stage, "folder-to-file"), "single")
	writeTestFile(t, filepath.Join(stage, "new.txt"), "fresh")

	conflicts, err := collectConflicts(target, stage)
	if err != nil {
		t.Fatalf("collectConflicts error: %v", err)
	}

	want := []string{
		"folder-to-file/inside.txt",
		"merge/swap",
		"replace-dir/nested.txt",
		"same.txt",
	}
	if !reflect.DeepEqual(conflicts, want) {
		t.Fatalf("unexpected conflicts:\nwant %#v\ngot  %#v", want, conflicts)
	}
}

func TestCollectConflictsWithEmptyDirectoryReplacement(t *testing.T) {
	target := t.TempDir()
	stage := t.TempDir()

	if err := os.MkdirAll(filepath.Join(target, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	writeTestFile(t, filepath.Join(stage, "empty"), "replacement")

	conflicts, err := collectConflicts(target, stage)
	if err != nil {
		t.Fatalf("collectConflicts error: %v", err)
	}

	want := []string{"empty/"}
	if !reflect.DeepEqual(conflicts, want) {
		t.Fatalf("unexpected conflicts:\nwant %#v\ngot  %#v", want, conflicts)
	}
}

func TestApplyStage(t *testing.T) {
	target := t.TempDir()
	stage := t.TempDir()

	writeTestFile(t, filepath.Join(target, "file-to-dir"), "old")
	writeTestFile(t, filepath.Join(target, "dir-to-file", "nested.txt"), "nested")
	writeTestFile(t, filepath.Join(target, "merge", "old.txt"), "old")

	writeTestFile(t, filepath.Join(stage, "file-to-dir", "child.txt"), "child")
	writeTestFile(t, filepath.Join(stage, "dir-to-file"), "flat")
	writeTestFile(t, filepath.Join(stage, "merge", "new.txt"), "new")

	if err := applyStage(target, stage); err != nil {
		t.Fatalf("applyStage error: %v", err)
	}

	assertFileContent(t, filepath.Join(target, "file-to-dir", "child.txt"), "child")
	assertFileContent(t, filepath.Join(target, "dir-to-file"), "flat")
	assertFileContent(t, filepath.Join(target, "merge", "old.txt"), "old")
	assertFileContent(t, filepath.Join(target, "merge", "new.txt"), "new")
}

func TestHandleBrowseServesIndexHTMLAsFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "index.html"), "<!doctype html><h1>remote</h1>")

	srv := &server{root: root}
	request := httptest.NewRequest(http.MethodGet, "/index.html", nil)
	response := httptest.NewRecorder()

	srv.handleBrowse(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("expected text/plain content type, got %q", contentType)
	}
	if nosniff := response.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Fatalf("expected nosniff header, got %q", nosniff)
	}
	if body := response.Body.String(); !strings.Contains(body, "<!doctype html><h1>remote</h1>") {
		t.Fatalf("expected index.html content, got %q", body)
	}
}

func TestHandleBrowseDoesNotForceBinaryToText(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "image.bin")
	if err := os.WriteFile(binaryPath, []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x01}, 0o644); err != nil {
		t.Fatalf("write binary file: %v", err)
	}

	srv := &server{root: root}
	request := httptest.NewRequest(http.MethodGet, "/image.bin", nil)
	response := httptest.NewRecorder()

	srv.handleBrowse(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("expected non-text content type, got %q", contentType)
	}
	if !bytes.Equal(response.Body.Bytes(), []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x01}) {
		t.Fatalf("unexpected binary body: %#v", response.Body.Bytes())
	}
}

func TestFormatHelpers(t *testing.T) {
	if got := formatSize(1536); got != "1.5K" {
		t.Fatalf("formatSize got %q", got)
	}
	if got := formatSize(1024); got != "1K" {
		t.Fatalf("formatSize exact got %q", got)
	}

	when := time.Date(2026, time.April, 22, 13, 5, 9, 0, time.UTC)
	if got := formatModifiedTime(when); got != "2026年 4月22日  1:05:09 PM" {
		t.Fatalf("formatModifiedTime got %q", got)
	}
}

func writeTestFile(t *testing.T, filePath, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filePath, err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", filePath, err)
	}
}

func assertFileContent(t *testing.T, filePath, want string) {
	t.Helper()
	bytes, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read %s: %v", filePath, err)
	}
	if string(bytes) != want {
		t.Fatalf("content mismatch for %s: want %q got %q", filePath, want, string(bytes))
	}
}
