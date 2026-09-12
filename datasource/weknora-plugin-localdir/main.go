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
			// 单个文件读不出来（权限、被占用、超限…）不该拖垮整轮同步，
			// 以失败占位项上报后继续处理其余文件。
			items = append(items, failureItem(file, err))
			continue
		}
		items = append(items, item)
	}
	return items, nil
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
			// 读取失败的文件**在源端仍然存在**：必须把它算进 seen 并保留旧哈希，
			// 否则下面的删除扫描会把它当作"已消失"而发出删除墓碑 —— 那会因为
			// 一次临时读取失败就删掉用户已经入库的文档。
			externalID := externalIDFor(file)
			seen[externalID] = struct{}{}
			if old, ok := previous.Files[externalID]; ok {
				current.Files[externalID] = old
			}
			items = append(items, failureItem(file, err))
			continue
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

// maxFileBytes 限制单个文件的大小。宿主与 SDK 两侧的 gRPC 消息上限都是 50 MB，
// 而批量路径把所有条目打包进**同一个**响应，所以必须留足余量。超过上限的文件会以
// 「失败占位项」上报（见 failureItem），而不是把整轮同步撑爆成一条难排查的 gRPC 错误。
//
// 用 var 而非 const 是为了让测试能把上限调小到几字节，从而在不写 32 MiB 数据的前提下
// 覆盖「超限 / 读取失败」这两条分支。
var maxFileBytes int64 = 32 << 20 // 32 MiB

// externalIDFor 是文件 ExternalID 的唯一构造处：readItem 与 failureItem 必须生成
// 完全一致的 ID，否则失败项会和真实项对不上号。
func externalIDFor(relative string) string {
	return "file:" + filepath.ToSlash(relative)
}

// failureItem 以「失败占位项」上报单个文件的失败：不带任何内容，只用
// Metadata["error"] 说明原因。这是宿主与内置连接器（yuque、feishu）共用的约定，
// 宿主会把它计入 result.Failed、写进同步日志，并因为 Failed > 0 而**保留上一轮
// 的 cursor**，下一轮增量重拉同一批 —— 该文件会被重试，而不是被永久跳过。
func failureItem(relative string, cause error) pluginapi.FetchedItem {
	return pluginapi.FetchedItem{
		ExternalID: externalIDFor(relative),
		Title:      filepath.Base(relative),
		FileName:   filepath.Base(relative),
		Metadata: map[string]string{
			"path":  filepath.ToSlash(relative),
			"error": cause.Error(),
		},
	}
}

func readItem(root, relative string) (pluginapi.FetchedItem, error) {
	path := filepath.Join(root, relative)
	info, err := os.Stat(path)
	if err != nil {
		return pluginapi.FetchedItem{}, err
	}
	// 超限文件在读取前就拒绝：先读进内存再判断只会让宿主更早撞上 OOM / 消息上限。
	if info.Size() > maxFileBytes {
		return pluginapi.FetchedItem{}, fmt.Errorf(
			"file %s is %d bytes, over the %d byte limit", filepath.ToSlash(relative), info.Size(), maxFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return pluginapi.FetchedItem{}, err
	}
	return pluginapi.FetchedItem{
		ExternalID: externalIDFor(relative),
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
