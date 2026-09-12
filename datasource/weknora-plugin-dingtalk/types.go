package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// dingtalkConfig holds the resolved plugin configuration.
//
// Base URL 不在这里：插件恒用官方端点（见 client.go 的 cfgBaseURL）。
type dingtalkConfig struct {
	AppKey    string
	AppSecret string
	UnionID   string
}

// workspace is a DingTalk wiki workspace (knowledge base).
type workspace struct {
	WorkspaceID string `json:"workspaceId"`
	RootNodeID  string `json:"rootNodeId"`
	Name        string `json:"name"`
	Type        string `json:"type"`
}

// wikiNode is a node (file or folder) inside a DingTalk wiki.
type wikiNode struct {
	NodeID       string `json:"nodeId"`
	WorkspaceID  string `json:"workspaceId"`
	Name         string `json:"name"`
	Type         string `json:"type"`      // FILE / FOLDER
	Category     string `json:"category"`  // ALIDOC / DOCUMENT / IMAGE / ...
	Extension    string `json:"extension"` // adoc / docx / pdf / ...
	ParentNodeID string `json:"parentNodeId"`
	HasChildren  bool   `json:"hasChildren"`
	ModifiedTime string `json:"modifiedTime"`
	DocKey       string `json:"docKey"`

	// ModifiedTimestamp is the reliable millisecond timestamp DingTalk
	// returns alongside the (second-less, non-RFC3339) modifiedTime string.
	ModifiedTimestamp int64 `json:"modifiedTimestamp"`
}

// accessTokenResponse handles DingTalk's camelCase and snake_case variants.
type accessTokenResponse struct {
	AccessToken string `json:"accessToken"`
	ExpireIn    int64  `json:"expireIn"`
	Code        string `json:"code"`
	Message     string `json:"message"`
}

type workspaceListResponse struct {
	Workspaces []workspace `json:"workspaces"`
	NextToken  string      `json:"nextToken"`
}

type nodeListResponse struct {
	Nodes     []wikiNode `json:"nodes"`
	NextToken string      `json:"nextToken"`
}

// docBlocksResponse carries the raw JSON blocks of an ALIDOC document.
//
// The DingTalk API wraps the actual payload under "result", with a top-level
// "success" flag. Missing the "result" wrapper causes silent empty responses
// that put the paginator into an infinite loop.
type docBlocksResponse struct {
	Result struct {
		Data []jsonRaw `json:"data"`
	} `json:"result"`
}

type jsonRaw = json.RawMessage

// apiErrorBody unifies DingTalk error shapes (code vs errcode).
type apiErrorBody struct {
	Code    jsonScalar `json:"code"`
	ErrCode jsonScalar `json:"errcode"`
	Message string     `json:"message"`
}

// jsonScalar accepts string or numeric error codes.
type jsonScalar string

func (s *jsonScalar) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*s = jsonScalar(str)
		return nil
	}
	var num float64
	if err := json.Unmarshal(data, &num); err == nil {
		*s = jsonScalar(fmt.Sprintf("%v", num))
		return nil
	}
	return fmt.Errorf("jsonScalar: unsupported type")
}

// --- sync cursor state ---

// dingTalkCursor is the plugin's incremental-sync cursor.
type dingTalkCursor struct {
	Resources map[string]*resourceState `json:"resources"`
}

type resourceState struct {
	Nodes     map[string]*nodeState `json:"nodes"`
	Complete  bool                  `json:"complete"`
	SyncedAt  string                `json:"synced_at,omitempty"`
	Workspace string                `json:"workspace,omitempty"`
}

type nodeState struct {
	ModifiedTime string `json:"modified_time"`
}

func newCursor() dingTalkCursor {
	return dingTalkCursor{Resources: map[string]*resourceState{}}
}

// --- helpers ---

// parseDingTalkTime parses the several timestamp shapes DingTalk emits,
// including the second-less "2026-07-15T13:33Z" form that Go's RFC3339
// layouts reject (they require a seconds field before the zone).
func parseDingTalkTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04Z07:00", // second-less with zone (DingTalk modifiedTime)
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, value); err == nil {
			return t
		}
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		if n > 100_000_000_000 {
			return time.UnixMilli(n)
		}
		if n > 1_000_000_000 {
			return time.Unix(n, 0)
		}
	}
	return time.Time{}
}

// normalizedModifiedTime returns an RFC3339Nano string the host can always
// parse. It prefers the millisecond timestamp (unambiguous) and falls back to
// parsing the human-readable modifiedTime.
func normalizedModifiedTime(n wikiNode) string {
	if n.ModifiedTimestamp > 0 {
		return time.UnixMilli(n.ModifiedTimestamp).UTC().Format(time.RFC3339Nano)
	}
	if t := parseDingTalkTime(n.ModifiedTime); !t.IsZero() {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return ""
}

func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
	)
	name = replacer.Replace(name)
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

// isSyncableDoc reports whether a node is a DingTalk native online document
// (.adoc). We deliberately require extension == "adoc" rather than
// category == "ALIDOC", because DingTalk also reports spreadsheets (axls),
// multi-dimensional tables (able) and other ALIDOC objects that the blocks
// API rejects with "Target document should be doc.".
func isSyncableDoc(n wikiNode) bool {
	if n.Type != "FILE" {
		return false
	}
	return strings.ToLower(strings.TrimPrefix(n.Extension, ".")) == "adoc"
}

func redact(value string) string {
	if len(value) <= 8 {
		return "****"
	}
	return value[:4] + "****" + value[len(value)-4:]
}
