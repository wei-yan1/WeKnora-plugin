package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/index"
	client "github.com/milvus-io/milvus/client/v2/milvusclient"

	"github.com/Tencent/WeKnora/pkg/pluginapi"
)

// 字段名常量，对齐主仓内建 Milvus 引擎的 schema。
const (
	defaultCollectionName = "weknora_embeddings"
	fieldID               = "id"
	fieldEmbedding        = "embedding"
	fieldContent          = "content"
	fieldContentSparse    = "content_sparse"
	fieldSourceID         = "source_id"
	fieldSourceType       = "source_type"
	fieldChunkID          = "chunk_id"
	fieldKnowledgeID      = "knowledge_id"
	fieldKnowledgeBaseID  = "knowledge_base_id"
	fieldTagID            = "tag_id"
	fieldIsEnabled        = "is_enabled"
)

// allFields 是 Query/结果转换时会读取的所有字段。
var allFields = []string{
	fieldID, fieldContent, fieldSourceID, fieldSourceType, fieldChunkID,
	fieldKnowledgeID, fieldKnowledgeBaseID, fieldTagID, fieldIsEnabled, fieldEmbedding,
}

// record 是一条完整记录（Query 结果 + Upsert 输入）。
type record struct {
	ID              string
	Content         string
	SourceID        string
	SourceType      int64
	ChunkID         string
	KnowledgeID     string
	KnowledgeBaseID string
	TagID           string
	Embedding       []float32
	IsEnabled       bool
}

// toMetadata 把记录转成宿主期望的 metadata（全字符串）。
func (r *record) toMetadata() map[string]string {
	return map[string]string{
		fieldSourceID:        r.SourceID,
		fieldSourceType:      strconv.FormatInt(r.SourceType, 10),
		fieldChunkID:         r.ChunkID,
		fieldKnowledgeID:     r.KnowledgeID,
		fieldKnowledgeBaseID: r.KnowledgeBaseID,
		fieldTagID:           r.TagID,
		fieldIsEnabled:       strconv.FormatBool(r.IsEnabled),
	}
}

// milvuxBackend 实现 pluginapi.RetrieverBackend。
type milvuxBackend struct {
	client             *client.Client
	collectionBaseName string
	// initialized 缓存已建好的 collection 维度，避免重复 HasCollection/Load。
	initialized sync.Map // dimension(int) -> struct{}
}

// openBackend 是 RetrieverProvider.Open：建立 Milvus 连接并返回一个独立 Store 实例。
func openBackend(ctx context.Context, config pluginapi.RetrieverStoreConfig) (pluginapi.RetrieverBackend, error) {
	addr := getString(config.Settings, "addr")
	if addr == "" {
		return nil, fmt.Errorf("settings.addr is required")
	}

	mc := client.ClientConfig{Address: addr}
	if v := getString(config.Settings, "database"); v != "" {
		mc.DBName = v
	}
	if v := getString(config.Credentials, "username"); v != "" {
		mc.Username = v
	}
	if v := getString(config.Credentials, "password"); v != "" {
		mc.Password = v
	}

	c, err := client.New(ctx, &mc)
	if err != nil {
		return nil, fmt.Errorf("create milvus client: %w", err)
	}

	collectionName := getString(config.IndexConfig, "collection_name")
	if collectionName == "" {
		collectionName = defaultCollectionName
	}

	return &milvuxBackend{client: c, collectionBaseName: collectionName}, nil
}

func (b *milvuxBackend) getCollectionName(dimension int) string {
	return fmt.Sprintf("%s_%d", b.collectionBaseName, dimension)
}

// listCollections 返回所有属于本 Store 的 collection（前缀匹配）。
func (b *milvuxBackend) listCollections(ctx context.Context) ([]string, error) {
	all, err := b.client.ListCollections(ctx, client.NewListCollectionOption())
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	prefix := b.collectionBaseName + "_"
	var out []string
	for _, name := range all {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	return out, nil
}

// ensureCollection 惰性创建并加载指定维度的 collection。
func (b *milvuxBackend) ensureCollection(ctx context.Context, dimension int) error {
	collectionName := b.getCollectionName(dimension)
	if _, ok := b.initialized.Load(dimension); ok {
		return nil
	}

	has, err := b.client.HasCollection(ctx, client.NewHasCollectionOption(collectionName))
	if err != nil {
		return fmt.Errorf("check collection existence: %w", err)
	}

	if !has {
		schema := &entity.Schema{
			CollectionName: collectionName,
			AutoID:         false,
			Fields: []*entity.Field{
				entity.NewField().WithName(fieldID).WithDataType(entity.FieldTypeVarChar).WithIsPrimaryKey(true).WithMaxLength(1024),
				entity.NewField().WithName(fieldEmbedding).WithDataType(entity.FieldTypeFloatVector).WithDim(int64(dimension)),
				entity.NewField().WithName(fieldContent).WithDataType(entity.FieldTypeVarChar).WithMaxLength(65535).WithEnableAnalyzer(true).WithEnableMatch(true),
				entity.NewField().WithName(fieldContentSparse).WithDataType(entity.FieldTypeSparseVector),
				entity.NewField().WithName(fieldSourceID).WithDataType(entity.FieldTypeVarChar).WithMaxLength(255),
				entity.NewField().WithName(fieldSourceType).WithDataType(entity.FieldTypeInt64),
				entity.NewField().WithName(fieldChunkID).WithDataType(entity.FieldTypeVarChar).WithMaxLength(255),
				entity.NewField().WithName(fieldKnowledgeID).WithDataType(entity.FieldTypeVarChar).WithMaxLength(255),
				entity.NewField().WithName(fieldKnowledgeBaseID).WithDataType(entity.FieldTypeVarChar).WithMaxLength(255),
				entity.NewField().WithName(fieldTagID).WithDataType(entity.FieldTypeVarChar).WithMaxLength(255),
				entity.NewField().WithName(fieldIsEnabled).WithDataType(entity.FieldTypeBool),
			},
		}
		// BM25 Function：content -> content_sparse（稀疏向量，用于关键词检索）。
		schema.WithFunction(entity.NewFunction().
			WithName("text_bm25_emb").
			WithInputFields(fieldContent).
			WithOutputFields(fieldContentSparse).
			WithType(entity.FunctionTypeBM25))

		indexOpts := []client.CreateIndexOption{
			client.NewCreateIndexOption(collectionName, fieldEmbedding, index.NewHNSWIndex(entity.COSINE, 16, 128)),
			client.NewCreateIndexOption(collectionName, fieldContentSparse, index.NewAutoIndex(entity.BM25)),
		}
		for _, f := range []string{fieldChunkID, fieldKnowledgeID, fieldKnowledgeBaseID, fieldSourceID, fieldIsEnabled} {
			indexOpts = append(indexOpts, client.NewCreateIndexOption(collectionName, f, index.NewAutoIndex(entity.IP)))
		}

		createOpt := client.NewCreateCollectionOption(collectionName, schema).WithIndexOptions(indexOpts...)
		if err := b.client.CreateCollection(ctx, createOpt); err != nil {
			return fmt.Errorf("create collection: %w", err)
		}
	}

	loadTask, err := b.client.LoadCollection(ctx, client.NewLoadCollectionOption(collectionName))
	if err != nil {
		return fmt.Errorf("load collection: %w", err)
	}
	if err := loadTask.Await(ctx); err != nil {
		return fmt.Errorf("await load collection: %w", err)
	}

	b.initialized.Store(dimension, struct{}{})
	return nil
}

// BatchPut 按维度分组写入。
func (b *milvuxBackend) BatchPut(ctx context.Context, records []pluginapi.RetrieverRecord) error {
	byDim := make(map[int][]pluginapi.RetrieverRecord)
	for _, r := range records {
		if len(r.Embedding) == 0 {
			continue
		}
		d := len(r.Embedding)
		byDim[d] = append(byDim[d], r)
	}
	for dim, rs := range byDim {
		if err := b.ensureCollection(ctx, dim); err != nil {
			return err
		}
		if err := b.upsertRecords(ctx, b.getCollectionName(dim), rs); err != nil {
			return err
		}
	}
	return nil
}

// Search 按 RetrieverType 分发。
func (b *milvuxBackend) Search(ctx context.Context, req pluginapi.RetrieverSearchRequest) ([]pluginapi.RetrieverHit, error) {
	switch req.RetrieverType {
	case "vector":
		return b.vectorSearch(ctx, req)
	case "keywords":
		return b.keywordsSearch(ctx, req)
	default:
		return nil, fmt.Errorf("invalid retriever type: %q", req.RetrieverType)
	}
}

// vectorSearch 用 HNSW 做向量相似度检索，分数归一到 [0,1]。
func (b *milvuxBackend) vectorSearch(ctx context.Context, req pluginapi.RetrieverSearchRequest) ([]pluginapi.RetrieverHit, error) {
	dim := len(req.Embedding)
	if dim == 0 {
		return nil, fmt.Errorf("empty query embedding")
	}
	collectionName := b.getCollectionName(dim)

	has, err := b.client.HasCollection(ctx, client.NewHasCollectionOption(collectionName))
	if err != nil {
		return nil, fmt.Errorf("check collection: %w", err)
	}
	if !has {
		return nil, nil // collection 不存在 → 空结果
	}

	expr := b.buildFilterExpr(req.Filter)
	searchOpt := client.NewSearchOption(collectionName, req.TopK, []entity.Vector{entity.FloatVector(req.Embedding)}).
		WithANNSField(fieldEmbedding).
		WithOutputFields("*")
	if req.Threshold > 0 {
		ann := index.NewCustomAnnParam()
		ann.WithRadius(req.Threshold)
		searchOpt.WithAnnParam(ann)
	}
	if expr != "" {
		searchOpt.WithFilter(expr)
	}

	resultSet, err := b.client.Search(ctx, searchOpt)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}

	records, scores, err := convertSearchResult(resultSet)
	if err != nil {
		return nil, err
	}
	hits := make([]pluginapi.RetrieverHit, 0, len(records))
	for i, r := range records {
		// COSINE 分数值域 [-1,1]，归一到 [0,1]（similarity_higher_better 约定）。
		score := (scores[i] + 1) / 2
		if score < 0 {
			score = 0
		}
		if score > 1 {
			score = 1
		}
		hits = append(hits, pluginapi.RetrieverHit{RecordID: r.ID, Score: score, Metadata: r.toMetadata()})
	}
	return hits, nil
}

// keywordsSearch 用 BM25 稀疏向量检索，遍历所有维度 collection。
func (b *milvuxBackend) keywordsSearch(ctx context.Context, req pluginapi.RetrieverSearchRequest) ([]pluginapi.RetrieverHit, error) {
	collections, err := b.listCollections(ctx)
	if err != nil {
		return nil, err
	}

	expr := b.buildFilterExpr(req.Filter)
	var hits []pluginapi.RetrieverHit
	for _, collectionName := range collections {
		searchOpt := client.NewSearchOption(collectionName, req.TopK, []entity.Vector{entity.Text(req.Query)}).
			WithANNSField(fieldContentSparse).
			WithOutputFields("*")
		if expr != "" {
			searchOpt.WithFilter(expr)
		}
		resultSet, err := b.client.Search(ctx, searchOpt)
		if err != nil {
			return nil, fmt.Errorf("keywords search: %w", err)
		}
		records, _, err := convertSearchResult(resultSet)
		if err != nil {
			return nil, err
		}
		for _, r := range records {
			// 关键词命中即回（与内建 Milvus 一致），分数交给宿主 RRF 的 rank 处理。
			hits = append(hits, pluginapi.RetrieverHit{RecordID: r.ID, Score: 1.0, Metadata: r.toMetadata()})
		}
	}
	if len(hits) > req.TopK {
		hits = hits[:req.TopK]
	}
	return hits, nil
}

// Delete 按 record_ids 或 filter 删除。宿主主要走 filter（chunk_id / source_id / knowledge_id）。
func (b *milvuxBackend) Delete(ctx context.Context, recordIDs []string, filter map[string][]string) error {
	collections, err := b.listCollections(ctx)
	if err != nil {
		return err
	}
	for _, collectionName := range collections {
		if len(recordIDs) > 0 {
			if _, err := b.client.Delete(ctx, client.NewDeleteOption(collectionName).WithStringIDs(fieldID, recordIDs)); err != nil {
				return fmt.Errorf("delete by ids: %w", err)
			}
		}
		for field, values := range filter {
			if len(values) == 0 {
				continue
			}
			actual := strings.TrimPrefix(field, "exclude_")
			if _, err := b.client.Delete(ctx, client.NewDeleteOption(collectionName).WithStringIDs(actual, values)); err != nil {
				return fmt.Errorf("delete by filter %s: %w", field, err)
			}
		}
	}
	return nil
}

// Patch 按 filter 定位记录，更新 patch 里的字段（enabled / tag_id），再 Upsert 回去。
func (b *milvuxBackend) Patch(ctx context.Context, recordIDs []string, filter map[string][]string, patch map[string]string) error {
	if len(patch) == 0 {
		return nil
	}
	collections, err := b.listCollections(ctx)
	if err != nil {
		return err
	}

	// 定位表达式：优先 record_ids（主键），否则 filter。
	var expr string
	if len(recordIDs) > 0 {
		quoted := make([]string, len(recordIDs))
		for i, id := range recordIDs {
			quoted[i] = strconv.Quote(id)
		}
		expr = fmt.Sprintf("%s in [%s]", fieldID, strings.Join(quoted, ","))
	} else {
		expr = b.buildFilterExprNoEnabled(filter)
	}

	for _, collectionName := range collections {
		records, err := b.queryRecords(ctx, collectionName, expr)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			continue
		}
		for _, r := range records {
			if v, ok := patch["enabled"]; ok {
				r.IsEnabled = v == "true"
			}
			if v, ok := patch["tag_id"]; ok {
				r.TagID = v
			}
		}
		if err := b.upsertRecordObjects(ctx, collectionName, records); err != nil {
			return err
		}
	}
	return nil
}

// Close 释放 Milvus 连接。
func (b *milvuxBackend) Close(ctx context.Context) error {
	if b.client == nil {
		return nil
	}
	return b.client.Close(ctx)
}

// buildFilterExpr 把宿主 filter 转成 Milvus 表达式，并强制过滤禁用记录。
func (b *milvuxBackend) buildFilterExpr(filter map[string][]string) string {
	expr := b.buildFilterExprNoEnabled(filter)
	if expr == "" {
		return fmt.Sprintf("%s == true", fieldIsEnabled)
	}
	return expr + fmt.Sprintf(" and %s == true", fieldIsEnabled)
}

// buildFilterExprNoEnabled 只转换 filter，不加 is_enabled 条件。
func (b *milvuxBackend) buildFilterExprNoEnabled(filter map[string][]string) string {
	var parts []string
	for field, values := range filter {
		if len(values) == 0 {
			continue
		}
		actual := field
		op := "in"
		if strings.HasPrefix(field, "exclude_") {
			actual = strings.TrimPrefix(field, "exclude_")
			op = "not in"
		}
		quoted := make([]string, len(values))
		for i, v := range values {
			quoted[i] = strconv.Quote(v)
		}
		parts = append(parts, fmt.Sprintf("%s %s [%s]", actual, op, strings.Join(quoted, ",")))
	}
	return strings.Join(parts, " and ")
}

// upsertRecords 把 pluginapi.RetrieverRecord 列式写入 Milvus。
func (b *milvuxBackend) upsertRecords(ctx context.Context, collectionName string, records []pluginapi.RetrieverRecord) error {
	objs := make([]*record, 0, len(records))
	for _, r := range records {
		objs = append(objs, &record{
			ID:              r.RecordID,
			Content:         r.Content,
			SourceID:        r.Metadata[fieldSourceID],
			SourceType:      parseInt64(r.Metadata[fieldSourceType]),
			ChunkID:         r.Metadata[fieldChunkID],
			KnowledgeID:     r.Metadata[fieldKnowledgeID],
			KnowledgeBaseID: r.Metadata[fieldKnowledgeBaseID],
			TagID:           r.Metadata[fieldTagID],
			Embedding:       r.Embedding,
			IsEnabled:       parseBool(r.Metadata[fieldIsEnabled]),
		})
	}
	return b.upsertRecordObjects(ctx, collectionName, objs)
}

// upsertRecordObjects 把 []*record 列式写入 Milvus。
func (b *milvuxBackend) upsertRecordObjects(ctx context.Context, collectionName string, records []*record) error {
	if len(records) == 0 {
		return nil
	}
	n := len(records)
	ids := make([]string, 0, n)
	embeddings := make([][]float32, 0, n)
	contents := make([]string, 0, n)
	sourceIDs := make([]string, 0, n)
	sourceTypes := make([]int64, 0, n)
	chunkIDs := make([]string, 0, n)
	knowledgeIDs := make([]string, 0, n)
	kbIDs := make([]string, 0, n)
	tagIDs := make([]string, 0, n)
	enableds := make([]bool, 0, n)
	dim := 0
	for _, r := range records {
		ids = append(ids, r.ID)
		embeddings = append(embeddings, r.Embedding)
		contents = append(contents, r.Content)
		sourceIDs = append(sourceIDs, r.SourceID)
		sourceTypes = append(sourceTypes, r.SourceType)
		chunkIDs = append(chunkIDs, r.ChunkID)
		knowledgeIDs = append(knowledgeIDs, r.KnowledgeID)
		kbIDs = append(kbIDs, r.KnowledgeBaseID)
		tagIDs = append(tagIDs, r.TagID)
		enableds = append(enableds, r.IsEnabled)
		if len(r.Embedding) > 0 {
			dim = len(r.Embedding)
		}
	}
	opt := client.NewColumnBasedInsertOption(collectionName).
		WithVarcharColumn(fieldID, ids).
		WithFloatVectorColumn(fieldEmbedding, dim, embeddings).
		WithVarcharColumn(fieldContent, contents).
		WithVarcharColumn(fieldSourceID, sourceIDs).
		WithInt64Column(fieldSourceType, sourceTypes).
		WithVarcharColumn(fieldChunkID, chunkIDs).
		WithVarcharColumn(fieldKnowledgeID, knowledgeIDs).
		WithVarcharColumn(fieldKnowledgeBaseID, kbIDs).
		WithVarcharColumn(fieldTagID, tagIDs).
		WithBoolColumn(fieldIsEnabled, enableds)
	if _, err := b.client.Upsert(ctx, opt); err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	return nil
}

// queryRecords 按表达式查询完整记录（含 embedding）。
func (b *milvuxBackend) queryRecords(ctx context.Context, collectionName, expr string) ([]*record, error) {
	queryOpt := client.NewQueryOption(collectionName).WithOutputFields("*")
	if expr != "" {
		queryOpt.WithFilter(expr)
	}
	resultSet, err := b.client.Query(ctx, queryOpt)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	return convertQueryResult(resultSet)
}

// convertSearchResult 把 Search 的 ResultSet 转成 []*record + 分数。
func convertSearchResult(resultSet []client.ResultSet) ([]*record, []float64, error) {
	if len(resultSet) == 0 {
		return nil, nil, nil
	}
	set := resultSet[0]
	n := set.Len()
	if n == 0 {
		return nil, nil, nil
	}
	records := make([]*record, n)
	for i := range records {
		records[i] = &record{}
	}
	scores := make([]float64, n)
	for i, s := range set.Scores {
		if i < n {
			scores[i] = float64(s)
		}
	}
	if err := fillRecords(records, set); err != nil {
		return nil, nil, err
	}
	return records, scores, nil
}

// convertQueryResult 把 Query 的 ResultSet 转成 []*record。
func convertQueryResult(resultSet client.ResultSet) ([]*record, error) {
	n := resultSet.Len()
	if n == 0 {
		return nil, nil
	}
	records := make([]*record, n)
	for i := range records {
		records[i] = &record{}
	}
	if err := fillRecords(records, resultSet); err != nil {
		return nil, err
	}
	return records, nil
}

// fillRecords 从 ResultSet 逐字段填充 records。
func fillRecords(records []*record, set client.ResultSet) error {
	for _, field := range allFields {
		col := set.GetColumn(field)
		if col == nil {
			continue
		}
		switch field {
		case fieldID:
			for i := 0; i < col.Len(); i++ {
				v, err := col.GetAsString(i)
				if err != nil {
					return err
				}
				records[i].ID = v
			}
		case fieldContent:
			for i := 0; i < col.Len(); i++ {
				v, err := col.GetAsString(i)
				if err != nil {
					return err
				}
				records[i].Content = v
			}
		case fieldSourceID:
			for i := 0; i < col.Len(); i++ {
				v, err := col.GetAsString(i)
				if err != nil {
					return err
				}
				records[i].SourceID = v
			}
		case fieldSourceType:
			for i := 0; i < col.Len(); i++ {
				v, err := col.GetAsInt64(i)
				if err != nil {
					return err
				}
				records[i].SourceType = v
			}
		case fieldChunkID:
			for i := 0; i < col.Len(); i++ {
				v, err := col.GetAsString(i)
				if err != nil {
					return err
				}
				records[i].ChunkID = v
			}
		case fieldKnowledgeID:
			for i := 0; i < col.Len(); i++ {
				v, err := col.GetAsString(i)
				if err != nil {
					return err
				}
				records[i].KnowledgeID = v
			}
		case fieldKnowledgeBaseID:
			for i := 0; i < col.Len(); i++ {
				v, err := col.GetAsString(i)
				if err != nil {
					return err
				}
				records[i].KnowledgeBaseID = v
			}
		case fieldTagID:
			for i := 0; i < col.Len(); i++ {
				v, err := col.GetAsString(i)
				if err != nil {
					return err
				}
				records[i].TagID = v
			}
		case fieldIsEnabled:
			for i := 0; i < col.Len(); i++ {
				v, err := col.GetAsBool(i)
				if err != nil {
					return err
				}
				records[i].IsEnabled = v
			}
		case fieldEmbedding:
			vecCol, ok := col.(*column.ColumnDoubleArray)
			if !ok {
				continue
			}
			for i := 0; i < vecCol.Len(); i++ {
				val, err := vecCol.Value(i)
				if err != nil {
					return err
				}
				emb := make([]float32, len(val))
				for j, v := range val {
					emb[j] = float32(v)
				}
				records[i].Embedding = emb
			}
		}
	}
	return nil
}

// getString 从 map[string]any 里取 string 值（宽松处理 JSON 反序列化后的数字类型）。
func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		return ""
	}
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func parseBool(s string) bool {
	v, _ := strconv.ParseBool(strings.TrimSpace(s))
	return v
}

func main() {
	provider := pluginapi.RetrieverProvider{
		PluginID:       "weknora.milvux",
		EngineType:     "milvux",
		Capabilities:   []string{"vector", "keywords", "filter"},
		ScoreSemantics: "similarity_higher_better",
		Open:           openBackend,
	}
	addr := os.Getenv("WEKNORA_PLUGIN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9776"
	}
	if err := pluginapi.ServeRetriever(context.Background(), addr, provider); err != nil {
		panic(err)
	}
}
