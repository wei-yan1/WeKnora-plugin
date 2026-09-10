package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/pkg/pluginapi"
)

func main() {
	address := flag.String("address", os.Getenv("WEKNORA_PLUGIN_ADDR"), "gRPC listen address")
	flag.Parse()
	if *address == "" {
		*address = "127.0.0.1:9777"
	}
	ctx := context.Background()
	plugin := localDirectoryHandler{}
	if err := pluginapi.Serve(ctx, *address, pluginapi.DataSourceHandler{
		PluginID: "weknora.localdir", Capabilities: []string{"incremental", "deletion_sync"},
		OnValidate:         plugin.Validate,
		OnListResources:    plugin.ListResources,
		OnFetchAll:         plugin.FetchAll,
		OnFetchIncremental: plugin.FetchIncremental,
		OnFetchAllStream: func(ctx context.Context, request pluginapi.Request, emit func(pluginapi.Response) error) error {
			return plugin.FetchAllStream(ctx, request, emit)
		},
		OnFetchIncrementalStream: func(ctx context.Context, request pluginapi.Request, emit func(pluginapi.Response) error) error {
			return plugin.FetchIncrementalStream(ctx, request, emit)
		},
	}); err != nil {
		panic(err)
	}
}

type localDirectoryHandler struct{}

type fileState struct {
	Hash string `json:"hash"`
}
type cursorState struct {
	Files map[string]fileState `json:"files"`
}

func (localDirectoryHandler) Validate(_ context.Context, request pluginapi.Request) error {
	root, _, err := settings(request)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("stat root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("root %q is not a directory", root)
	}
	return nil
}

func (h localDirectoryHandler) ListResources(ctx context.Context, request pluginapi.Request) ([]pluginapi.Resource, error) {
	root, _, err := settings(request)
	if err != nil {
		return nil, err
	}
	if err := h.Validate(ctx, request); err != nil {
		return nil, err
	}
	return []pluginapi.Resource{{ExternalID: "directory:/", Name: filepath.Base(root), Type: "directory"}}, nil
}

func (localDirectoryHandler) FetchAll(_ context.Context, request pluginapi.Request) ([]pluginapi.FetchedItem, error) {
	root, extensions, err := settings(request)
	if err != nil {
		return nil, err
	}
	files, err := scanFiles(root, extensions)
	if err != nil {
		return nil, err
	}
	items := make([]pluginapi.FetchedItem, 0, len(files))
	for _, file := range files {
		item, err := readItem(root, file)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (localDirectoryHandler) FetchAllStream(_ context.Context, request pluginapi.Request, emit func(pluginapi.Response) error) error {
	root, extensions, err := settings(request)
	if err != nil {
		return err
	}
	files, err := scanFiles(root, extensions)
	if err != nil {
		return err
	}
	current := cursorState{Files: make(map[string]fileState, len(files))}
	for _, file := range files {
		item, err := readItem(root, file)
		if err != nil {
			return err
		}
		current.Files[item.ExternalID] = fileState{Hash: hashBytes(item.Content)}
		if err := emit(pluginapi.Response{Items: []pluginapi.FetchedItem{item}}); err != nil {
			return err
		}
	}
	return emit(pluginapi.Response{Cursor: mapFromCursor(current)})
}

func (localDirectoryHandler) FetchIncremental(_ context.Context, request pluginapi.Request) ([]pluginapi.FetchedItem, map[string]any, error) {
	root, extensions, err := settings(request)
	if err != nil {
		return nil, nil, err
	}
	files, err := scanFiles(root, extensions)
	if err != nil {
		return nil, nil, err
	}
	previous := cursorState{Files: map[string]fileState{}}
	if request.Cursor != nil {
		if err := decodeMap(request.Cursor, &previous); err != nil {
			return nil, nil, err
		}
		if previous.Files == nil {
			previous.Files = map[string]fileState{}
		}
	}
	current := cursorState{Files: make(map[string]fileState, len(files))}
	items := make([]pluginapi.FetchedItem, 0)
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		item, err := readItem(root, file)
		if err != nil {
			return nil, nil, err
		}
		seen[item.ExternalID] = struct{}{}
		state := fileState{Hash: hashBytes(item.Content)}
		current.Files[item.ExternalID] = state
		if old, ok := previous.Files[item.ExternalID]; ok && old.Hash == state.Hash {
			continue
		}
		items = append(items, item)
	}
	for externalID := range previous.Files {
		if _, ok := seen[externalID]; !ok {
			items = append(items, pluginapi.FetchedItem{ExternalID: externalID, IsDeleted: true})
		}
	}
	return items, mapFromCursor(current), nil
}

func (localDirectoryHandler) FetchIncrementalStream(_ context.Context, request pluginapi.Request, emit func(pluginapi.Response) error) error {
	root, extensions, err := settings(request)
	if err != nil {
		return err
	}
	files, err := scanFiles(root, extensions)
	if err != nil {
		return err
	}
	previous := cursorState{Files: map[string]fileState{}}
	if request.Cursor != nil {
		if err := decodeMap(request.Cursor, &previous); err != nil {
			return err
		}
		if previous.Files == nil {
			previous.Files = map[string]fileState{}
		}
	}
	current := cursorState{Files: make(map[string]fileState, len(files))}
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		item, err := readItem(root, file)
		if err != nil {
			return err
		}
		seen[item.ExternalID] = struct{}{}
		state := fileState{Hash: hashBytes(item.Content)}
		current.Files[item.ExternalID] = state
		if old, ok := previous.Files[item.ExternalID]; ok && old.Hash == state.Hash {
			continue
		}
		if err := emit(pluginapi.Response{Items: []pluginapi.FetchedItem{item}}); err != nil {
			return err
		}
	}
	for externalID := range previous.Files {
		if _, ok := seen[externalID]; ok {
			continue
		}
		if err := emit(pluginapi.Response{Items: []pluginapi.FetchedItem{{ExternalID: externalID, IsDeleted: true}}}); err != nil {
			return err
		}
	}
	return emit(pluginapi.Response{Cursor: mapFromCursor(current)})
}

func settings(request pluginapi.Request) (string, map[string]any, error) {
	settings, _ := request.Config["settings"].(map[string]any)
	root, _ := settings["root"].(string)
	if root == "" {
		return "", nil, errors.New("settings.root is required")
	}
	return filepath.Clean(root), settings, nil
}

func scanFiles(root string, extensions map[string]any) ([]string, error) {
	allowed := map[string]struct{}{}
	if values, ok := extensions["include_extensions"].([]any); ok {
		for _, value := range values {
			if text, ok := value.(string); ok {
				allowed[strings.ToLower(text)] = struct{}{}
			}
		}
	}
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if len(allowed) > 0 {
			if _, ok := allowed[ext]; !ok {
				return nil
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	sort.Strings(files)
	return files, err
}

func readItem(root, relative string) (pluginapi.FetchedItem, error) {
	path := filepath.Join(root, relative)
	data, err := os.ReadFile(path)
	if err != nil {
		return pluginapi.FetchedItem{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return pluginapi.FetchedItem{}, err
	}
	return pluginapi.FetchedItem{
		ExternalID: "file:" + filepath.ToSlash(relative),
		Title:      filepath.Base(relative),
		FileName:   filepath.Base(relative),
		Content:    data,
		MIMEType:   mimeForPath(relative),
		UpdatedAt:  info.ModTime().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		Metadata: map[string]string{
			"path": filepath.ToSlash(relative),
			"size": fmt.Sprintf("%d", info.Size()),
		},
	}, nil
}

// mimeForPath maps a file extension to a MIME type, falling back to text/plain.
func mimeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".html", ".htm":
		return "text/html"
	case ".csv":
		return "text/csv"
	case ".txt", "":
		return "text/plain"
	default:
		return "text/plain"
	}
}

func hashBytes(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func mapFromCursor(cursor cursorState) map[string]any {
	result := map[string]any{"files": map[string]any{}}
	files := result["files"].(map[string]any)
	for id, state := range cursor.Files {
		files[id] = map[string]any{"hash": state.Hash}
	}
	return result
}
func decodeMap(value map[string]any, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
