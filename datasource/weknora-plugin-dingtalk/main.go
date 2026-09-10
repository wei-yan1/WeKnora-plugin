package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/pkg/pluginapi"
)

const pluginID = "weknora.dingtalk-datasource"

func main() {
	address := flag.String("address", os.Getenv("WEKNORA_PLUGIN_ADDR"), "gRPC listen address")
	flag.Parse()
	if *address == "" {
		*address = "127.0.0.1:9778"
	}

	httpClient := pluginapi.NewPluginHTTPClient()
	httpClient.Timeout = 60 * time.Second

	handler := &dingtalkHandler{client: newDingtalkClient(httpClient)}
	if err := pluginapi.Serve(context.Background(), *address, pluginapi.DataSourceHandler{
		PluginID:                   pluginID,
		Capabilities:               []string{"incremental"},
		OnValidate:                 handler.Validate,
		OnListResources:            handler.ListResources,
		OnResolveResourceAncestors: handler.ResolveResourceAncestors,
		OnFetchAll:                 handler.FetchAll,
		OnFetchIncremental:         handler.FetchIncremental,
		OnFetchAllStream:           handler.FetchAllStream,
		OnFetchIncrementalStream:   handler.FetchIncrementalStream,
	}); err != nil {
		panic(err)
	}
}

// dingtalkHandler implements the DingTalk Wiki (knowledge base) data source.
type dingtalkHandler struct {
	client *dingtalkClient
}

func loadConfig(request pluginapi.Request) (*dingtalkConfig, error) {
	credentials, ok := request.Config["credentials"].(map[string]any)
	if !ok || credentials == nil {
		return nil, errors.New("credentials is required")
	}
	cfg := &dingtalkConfig{BaseURL: "https://api.dingtalk.com"}
	if v, ok := credentials["app_key"].(string); ok {
		cfg.AppKey = strings.TrimSpace(v)
	}
	if v, ok := credentials["app_secret"].(string); ok {
		cfg.AppSecret = strings.TrimSpace(v)
	}
	if v, ok := credentials["union_id"].(string); ok {
		cfg.UnionID = strings.TrimSpace(v)
	}
	if cfg.AppKey == "" {
		return nil, errors.New("credentials.app_key is required")
	}
	if cfg.AppSecret == "" {
		return nil, errors.New("credentials.app_secret is required")
	}
	if cfg.UnionID == "" {
		return nil, errors.New("credentials.union_id is required")
	}
	return cfg, nil
}

// Validate checks credentials by listing workspaces.
func (h *dingtalkHandler) Validate(ctx context.Context, request pluginapi.Request) error {
	cfg, err := loadConfig(request)
	if err != nil {
		return err
	}
	if _, err := h.client.listWorkspaces(ctx, cfg); err != nil {
		return fmt.Errorf("validate dingtalk credentials: %w", err)
	}
	return nil
}

// ListResources lazily returns the wiki tree.
func (h *dingtalkHandler) ListResources(ctx context.Context, request pluginapi.Request) ([]pluginapi.Resource, error) {
	cfg, err := loadConfig(request)
	if err != nil {
		return nil, err
	}

	parent := request.ParentID
	if parent == "" {
		workspaces, err := h.client.listWorkspaces(ctx, cfg)
		if err != nil {
			return nil, err
		}
		resources := make([]pluginapi.Resource, 0, len(workspaces))
		for _, ws := range workspaces {
			id, err := encodeResourceRef(workspaceResourceRef(ws.WorkspaceID))
			if err != nil {
				return nil, err
			}
			resources = append(resources, pluginapi.Resource{
				ExternalID: id,
				Name:       ws.Name,
				Type:       "workspace",
				Metadata: map[string]string{
					"workspace_id": ws.WorkspaceID,
					"root_node_id": ws.RootNodeID,
					"kind":         ws.Type,
				},
			})
		}
		return resources, nil
	}

	ref, err := decodeResourceRef(parent)
	if err != nil {
		return nil, fmt.Errorf("invalid parent id: %w", err)
	}
	parentNodeID, err := h.resolveNodeID(ctx, cfg, ref)
	if err != nil {
		return nil, err
	}

	nodes, err := h.client.listNodes(ctx, cfg, parentNodeID)
	if err != nil {
		return nil, err
	}
	resources := make([]pluginapi.Resource, 0, len(nodes))
	for _, n := range nodes {
		childRef := childResourceRef(ref, n.NodeID)
		id, err := encodeResourceRef(childRef)
		if err != nil {
			return nil, err
		}
		typ := strings.ToLower(n.Type)
		if typ == "" {
			typ = "node"
		}
		resources = append(resources, pluginapi.Resource{
			ExternalID: id,
			Name:       n.Name,
			ParentID:   parent,
			Type:       typ,
			Metadata: map[string]string{
				"workspace_id":  n.WorkspaceID,
				"category":      n.Category,
				"extension":     n.Extension,
				"has_children":  fmt.Sprintf("%t", n.HasChildren),
				"modified_time": n.ModifiedTime,
			},
		})
	}
	return resources, nil
}

// ResolveResourceAncestors returns the ancestor ExternalIDs for a deep node so
// the picker can restore a saved selection without rescanning the whole tree.
func (h *dingtalkHandler) ResolveResourceAncestors(ctx context.Context, request pluginapi.Request) ([]string, error) {
	ref, err := decodeResourceRef(request.ParentID)
	if err != nil {
		return nil, err
	}
	return ancestorResourceIDs(ref)
}

// resolveNodeID maps a resourceRef to the DingTalk node id to list.
// Workspace-level refs resolve to the workspace root node.
func (h *dingtalkHandler) resolveNodeID(ctx context.Context, cfg *dingtalkConfig, ref resourceRef) (string, error) {
	if ref.NodeID != "" {
		return ref.NodeID, nil
	}
	workspaces, err := h.client.listWorkspaces(ctx, cfg)
	if err != nil {
		return "", err
	}
	for _, ws := range workspaces {
		if ws.WorkspaceID == ref.WorkspaceID {
			return ws.RootNodeID, nil
		}
	}
	return "", fmt.Errorf("workspace %q not found", ref.WorkspaceID)
}

// FetchAll synchronizes the selected resources (or all workspaces) fully.
func (h *dingtalkHandler) FetchAll(ctx context.Context, request pluginapi.Request) ([]pluginapi.FetchedItem, error) {
	cfg, err := loadConfig(request)
	if err != nil {
		return nil, err
	}
	_, items, _, err := h.sync(ctx, cfg, request.ResourceIDs, dingTalkCursor{}, false)
	return items, err
}

// FetchIncremental is the unary fallback for hosts without streaming.
func (h *dingtalkHandler) FetchIncremental(ctx context.Context, request pluginapi.Request) ([]pluginapi.FetchedItem, map[string]any, error) {
	cfg, err := loadConfig(request)
	if err != nil {
		return nil, nil, err
	}
	prev, err := decodeCursor(request.Cursor)
	if err != nil {
		return nil, nil, err
	}
	cursor, items, _, err := h.sync(ctx, cfg, request.ResourceIDs, prev, true)
	return items, encodeCursor(cursor), err
}

// FetchAllStream emits items (and warnings) via the streaming channel.
func (h *dingtalkHandler) FetchAllStream(ctx context.Context, request pluginapi.Request, emit func(pluginapi.Response) error) error {
	cfg, err := loadConfig(request)
	if err != nil {
		return err
	}
	_, items, warnings, err := h.sync(ctx, cfg, request.ResourceIDs, dingTalkCursor{}, false)
	if err != nil {
		return err
	}
	return emit(pluginapi.Response{Items: items, Warnings: warnings})
}

// FetchIncrementalStream emits items, cursor and warnings via streaming.
func (h *dingtalkHandler) FetchIncrementalStream(ctx context.Context, request pluginapi.Request, emit func(pluginapi.Response) error) error {
	cfg, err := loadConfig(request)
	if err != nil {
		return err
	}
	prev, err := decodeCursor(request.Cursor)
	if err != nil {
		return err
	}
	cursor, items, warnings, err := h.sync(ctx, cfg, request.ResourceIDs, prev, true)
	if err != nil {
		return err
	}
	return emit(pluginapi.Response{Items: items, Cursor: encodeCursor(cursor), Warnings: warnings})
}

// sync drives both full and incremental synchronization.
func (h *dingtalkHandler) sync(
	ctx context.Context,
	cfg *dingtalkConfig,
	resourceIDs []string,
	prev dingTalkCursor,
	incremental bool,
) (dingTalkCursor, []pluginapi.FetchedItem, []string, error) {
	next := newCursor()
	var items []pluginapi.FetchedItem
	var warnings []string

	workspaces, err := h.client.listWorkspaces(ctx, cfg)
	if err != nil {
		return next, nil, warnings, err
	}

	selectedRefs, err := h.resolveSelection(workspaces, resourceIDs)
	if err != nil {
		return next, nil, warnings, err
	}

	for _, ref := range selectedRefs {
		prevRes := prev.Resources[ref.WorkspaceID]
		cur := &resourceState{
			Nodes:     map[string]*nodeState{},
			Complete:  true,
			SyncedAt:  time.Now().UTC().Format(time.RFC3339),
			Workspace: ref.WorkspaceID,
		}

		docs, complete, err := h.collectScope(ctx, cfg, ref)
		if err != nil {
			if prevRes != nil {
				next.Resources[ref.WorkspaceID] = prevRes
			}
			warnings = append(warnings, fmt.Sprintf("workspace %s: %v", ref.WorkspaceID, err))
			continue
		}
		cur.Complete = complete

		currentDocs := map[string]bool{}
		for _, d := range docs {
			currentDocs[d.NodeID] = true
			state := &nodeState{ModifiedTime: d.ModifiedTime}

			if incremental && prevRes != nil && prevRes.Nodes != nil {
				if prevState, ok := prevRes.Nodes[d.NodeID]; ok && prevState.ModifiedTime == d.ModifiedTime {
					cur.Nodes[d.NodeID] = prevState
					continue
				}
			}

			markdown, err := h.fetchDocument(ctx, cfg, d)
			if err != nil {
				if prevRes != nil && prevRes.Nodes != nil {
					if prevState, ok := prevRes.Nodes[d.NodeID]; ok {
						cur.Nodes[d.NodeID] = prevState
					}
				}
				warnings = append(warnings, fmt.Sprintf("document %s (%s): %v", d.Name, d.NodeID, err))
				continue
			}
			cur.Nodes[d.NodeID] = state
			items = append(items, h.toItem(d, markdown))
		}

		if incremental && complete && prevRes != nil {
			for prevNodeID := range prevRes.Nodes {
				if _, stillExists := currentDocs[prevNodeID]; stillExists {
					continue
				}
				if _, already := cur.Nodes[prevNodeID]; already {
					continue
				}
				items = append(items, deletedItem(docRef{
					WorkspaceID:  ref.WorkspaceID,
					NodeID:       prevNodeID,
					Name:         prevNodeID,
					ModifiedTime: prevRes.Nodes[prevNodeID].ModifiedTime,
				}))
			}
		}

		next.Resources[ref.WorkspaceID] = cur
	}

	return next, items, warnings, nil
}

// docRef identifies a syncable document.
type docRef struct {
	WorkspaceID  string
	NodeID       string
	DocKey       string
	Name         string
	ModifiedTime string
}

// resolveSelection maps resource IDs (or empty) to workspace-level scopes.
func (h *dingtalkHandler) resolveSelection(workspaces []workspace, resourceIDs []string) ([]resourceRef, error) {
	if len(resourceIDs) == 0 {
		refs := make([]resourceRef, 0, len(workspaces))
		for _, ws := range workspaces {
			refs = append(refs, workspaceResourceRef(ws.WorkspaceID))
		}
		return refs, nil
	}
	refs := make([]resourceRef, 0, len(resourceIDs))
	for _, id := range resourceIDs {
		ref, err := decodeResourceRef(id)
		if err != nil {
			return nil, fmt.Errorf("invalid resource id %q: %w", id, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// collectScope gathers syncable ALIDOC documents under a resource scope.
//
// A workspace-level selection recurses the whole workspace. A node-level
// selection recurses only that node's subtree (folder) or returns that single
// document. The node's own type is resolved by listing its parent; when the
// parent is unknown we fall back to treating the node as a document scope.
func (h *dingtalkHandler) collectScope(ctx context.Context, cfg *dingtalkConfig, ref resourceRef) ([]docRef, bool, error) {
	if ref.NodeID == "" {
		startNodeID, err := h.resolveNodeID(ctx, cfg, ref)
		if err != nil {
			return nil, false, err
		}
		return h.collectNode(ctx, cfg, ref.WorkspaceID, startNodeID, 0)
	}

	// Node-level selection: resolve the node's type by listing its parent.
	var parentID string
	if len(ref.Ancestors) > 0 {
		parentID = ref.Ancestors[len(ref.Ancestors)-1]
	} else {
		wsRoot, err := h.resolveNodeID(ctx, cfg, workspaceResourceRef(ref.WorkspaceID))
		if err != nil {
			return nil, false, err
		}
		parentID = wsRoot
	}

	nodes, err := h.client.listNodes(ctx, cfg, parentID)
	if err != nil {
		return nil, false, err
	}
	for _, n := range nodes {
		if n.NodeID != ref.NodeID {
			continue
		}
		if n.Type == "FOLDER" {
			return h.collectNode(ctx, cfg, ref.WorkspaceID, n.NodeID, 0)
		}
		if isSyncableDoc(n) {
			return []docRef{{
				WorkspaceID:  ref.WorkspaceID,
				NodeID:       n.NodeID,
				DocKey:       docKeyOf(n),
				Name:         n.Name,
				ModifiedTime: normalizedModifiedTime(n),
			}}, true, nil
		}
		// A non-syncable file (uploaded docx/pdf etc.) yields nothing.
		return nil, true, nil
	}

	// Node not found in parent listing: fall back to treating it as a document.
	return []docRef{{
		WorkspaceID:  ref.WorkspaceID,
		NodeID:       ref.NodeID,
		DocKey:       ref.NodeID,
		Name:         ref.NodeID,
		ModifiedTime: "",
	}}, true, nil
}

// collectNode recursively collects syncable ALIDOC documents under a node.
func (h *dingtalkHandler) collectNode(ctx context.Context, cfg *dingtalkConfig, workspaceID, nodeID string, depth int) ([]docRef, bool, error) {
	if depth > maxResourceDepth {
		return nil, false, fmt.Errorf("dingtalk tree exceeds max depth %d", maxResourceDepth)
	}
	nodes, err := h.client.listNodes(ctx, cfg, nodeID)
	if err != nil {
		return nil, false, err
	}
	var docs []docRef
	for _, n := range nodes {
		if n.Type == "FOLDER" {
			sub, _, err := h.collectNode(ctx, cfg, workspaceID, n.NodeID, depth+1)
			if err != nil {
				return nil, false, err
			}
			docs = append(docs, sub...)
			continue
		}
		if isSyncableDoc(n) {
			docs = append(docs, docRef{
				WorkspaceID:  workspaceID,
				NodeID:       n.NodeID,
				DocKey:       docKeyOf(n),
				Name:         n.Name,
				ModifiedTime: normalizedModifiedTime(n),
			})
		}
	}
	return docs, true, nil
}

// fetchDocument downloads an ALIDOC's blocks and renders Markdown.
func (h *dingtalkHandler) fetchDocument(ctx context.Context, cfg *dingtalkConfig, d docRef) (string, error) {
	blocks, err := h.client.listDocBlocks(ctx, cfg, d.DocKey)
	if err != nil {
		return "", err
	}
	markdown, _ := renderDocumentMarkdown(d.Name, blocks)
	return markdown, nil
}

func (h *dingtalkHandler) toItem(d docRef, markdown string) pluginapi.FetchedItem {
	return pluginapi.FetchedItem{
		ExternalID: "dingtalk:doc:" + d.WorkspaceID + ":" + d.NodeID,
		Title:      d.Name,
		FileName:   sanitizeFileName(d.Name) + ".md",
		Content:    []byte(markdown),
		MIMEType:   "text/markdown",
		UpdatedAt:  d.ModifiedTime,
		URL:        "https://alidocs.dingtalk.com/i/nodes/" + d.NodeID,
		Metadata: map[string]string{
			"workspace_id": d.WorkspaceID,
			"node_id":      d.NodeID,
			"category":     "ALIDOC",
		},
	}
}

func deletedItem(d docRef) pluginapi.FetchedItem {
	return pluginapi.FetchedItem{
		ExternalID: "dingtalk:doc:" + d.WorkspaceID + ":" + d.NodeID,
		IsDeleted:  true,
	}
}

func docKeyOf(n wikiNode) string {
	if strings.TrimSpace(n.DocKey) != "" {
		return strings.TrimSpace(n.DocKey)
	}
	return n.NodeID
}

// --- cursor encode/decode ---

func encodeCursor(cursor dingTalkCursor) map[string]any {
	data, _ := json.Marshal(cursor)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}

func decodeCursor(cursor map[string]any) (dingTalkCursor, error) {
	if cursor == nil {
		return newCursor(), nil
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return newCursor(), err
	}
	var out dingTalkCursor
	if err := json.Unmarshal(data, &out); err != nil {
		return newCursor(), err
	}
	if out.Resources == nil {
		out.Resources = map[string]*resourceState{}
	}
	return out, nil
}
