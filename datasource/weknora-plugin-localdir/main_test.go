package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tencent/WeKnora/pkg/pluginapi"
	"github.com/stretchr/testify/require"
)

func TestIncrementalOnlyReturnsChangedFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.md"), []byte("a-v1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.md"), []byte("b-v1"), 0o644))
	request := pluginapi.Request{Config: map[string]any{"settings": map[string]any{"root": root, "include_extensions": []any{".md"}}}}
	h := localDirectoryHandler{}
	first, cursor, err := h.FetchIncremental(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, first, 2)
	request.Cursor = cursor
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.md"), []byte("a-v2"), 0o644))
	second, _, err := h.FetchIncremental(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, "file:a.md", second[0].ExternalID)
	require.Equal(t, []byte("a-v2"), second[0].Content)
}

func TestIncrementalReturnsDeletion(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644))
	request := pluginapi.Request{Config: map[string]any{"settings": map[string]any{"root": root}}}
	h := localDirectoryHandler{}
	_, cursor, err := h.FetchIncremental(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "a.txt")))
	request.Cursor = cursor
	items, _, err := h.FetchIncremental(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.True(t, items[0].IsDeleted)
	require.Equal(t, "file:a.txt", items[0].ExternalID)
}
