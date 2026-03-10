package logcollector

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiskStorageWriteRead(t *testing.T) {
	dir := t.TempDir()
	storage := NewDiskStorage(dir)

	err := storage.Write("ns1/runner-abc/runner/2025-01-01T00-00-00.log", strings.NewReader("log line 1\nlog line 2\n"))
	require.NoError(t, err)

	reader, err := storage.Read("ns1/runner-abc/runner/2025-01-01T00-00-00.log")
	require.NoError(t, err)
	defer reader.Close()

	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Contains(t, string(data), "log line 1")
	assert.Contains(t, string(data), "log line 2")
}

func TestDiskStorageDelete(t *testing.T) {
	dir := t.TempDir()
	storage := NewDiskStorage(dir)

	err := storage.Write("a/b.log", strings.NewReader("data"))
	require.NoError(t, err)

	err = storage.Delete("a/b.log")
	require.NoError(t, err)

	_, err = storage.Read("a/b.log")
	assert.Error(t, err)
}

func TestDiskStorageListPrefix(t *testing.T) {
	dir := t.TempDir()
	storage := NewDiskStorage(dir)

	storage.Write("ns1/runner-1/runner/a.log", strings.NewReader("data1"))
	storage.Write("ns1/runner-1/runner/b.log", strings.NewReader("data2"))
	storage.Write("ns2/runner-2/runner/c.log", strings.NewReader("data3"))

	files := storage.ListPrefix("ns1/runner-1/")
	assert.Len(t, files, 2)
	assert.Contains(t, files[0], "a.log")
	assert.Contains(t, files[1], "b.log")
}

func TestDiskStorageUsageBytes(t *testing.T) {
	dir := t.TempDir()
	storage := NewDiskStorage(dir)

	data := strings.Repeat("x", 1024)
	storage.Write("a.log", strings.NewReader(data))

	usage, err := storage.UsageBytes()
	require.NoError(t, err)
	assert.Equal(t, int64(1024), usage)
}

func TestDiskStorageOldestFiles(t *testing.T) {
	dir := t.TempDir()
	storage := NewDiskStorage(dir)

	storage.Write("old.log", strings.NewReader("old"))
	storage.Write("new.log", strings.NewReader("new"))

	files, err := storage.OldestFiles()
	require.NoError(t, err)
	assert.Len(t, files, 2)
}

func TestDiskStorageEmptyDir(t *testing.T) {
	dir := t.TempDir()
	storage := NewDiskStorage(dir)

	usage, err := storage.UsageBytes()
	require.NoError(t, err)
	assert.Equal(t, int64(0), usage)

	files := storage.ListPrefix("")
	assert.Empty(t, files)
}

func TestDiskStorageReadNonexistent(t *testing.T) {
	dir := t.TempDir()
	storage := NewDiskStorage(dir)

	_, err := storage.Read("does/not/exist.log")
	assert.Error(t, err)
}
