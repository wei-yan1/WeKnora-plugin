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

// shrinkLimitTo lowers maxFileBytes for the duration of a test so the oversized
// branch can be exercised without writing tens of megabytes.
func shrinkLimitTo(t *testing.T, limit int64) {
	t.Helper()
	previous := maxFileBytes
	maxFileBytes = limit
	t.Cleanup(func() { maxFileBytes = previous })
}

// TestUnreadableFileBecomesFailurePlaceholder pins the contract that a single
// file we cannot read must NOT abort the whole sync and must NOT be silently
// dropped: it is reported as a failure placeholder (no content + Metadata["error"])
// which the host counts as a failed document and retries on the next run.
func TestUnreadableFileBecomesFailurePlaceholder(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "ok.md"), []byte("k"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "big.md"), []byte("123456"), 0o644))
	shrinkLimitTo(t, 4)

	request := pluginapi.Request{Config: map[string]any{"settings": map[string]any{"root": root}}}
	items, _, err := localDirectoryHandler{}.FetchIncremental(context.Background(), request)
	require.NoError(t, err, "one oversized file must not fail the whole sync")
	require.Len(t, items, 2)

	byID := map[string]pluginapi.FetchedItem{}
	for _, item := range items {
		byID[item.ExternalID] = item
	}
	placeholder, ok := byID["file:big.md"]
	require.True(t, ok, "the oversized file must still be reported")
	require.Contains(t, placeholder.Metadata["error"], "over the 4 byte limit")
	require.Empty(t, placeholder.Content)
	require.False(t, placeholder.IsDeleted)
	require.Equal(t, []byte("k"), byID["file:ok.md"].Content)
}

// TestUnreadableFileIsNotTreatedAsDeleted is the important one: the file is still
// present in the source, so a transient read failure must never be turned into a
// deletion tombstone — that would delete the user's already-ingested document.
func TestUnreadableFileIsNotTreatedAsDeleted(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "large.md"), []byte("123456"), 0o644))
	request := pluginapi.Request{Config: map[string]any{"settings": map[string]any{"root": root}}}

	// First sync succeeds and records the file in the cursor.
	_, cursor, err := localDirectoryHandler{}.FetchIncremental(context.Background(), request)
	require.NoError(t, err)

	// Second sync can no longer read it (limit shrunk to simulate any read failure).
	shrinkLimitTo(t, 4)
	request.Cursor = cursor
	items, _, err := localDirectoryHandler{}.FetchIncremental(context.Background(), request)
	require.NoError(t, err)

	require.Len(t, items, 1, "only the failure placeholder is expected")
	require.Equal(t, "file:large.md", items[0].ExternalID)
	require.False(t, items[0].IsDeleted, "a read failure must not emit a deletion tombstone")
	require.NotEmpty(t, items[0].Metadata["error"])
	require.Empty(t, items[0].Content)
}
