package run

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// artifacts.go 是「构建产物契约」的领域层(FR-6 / Story 3.4)。
//
// 产物契约是 Epic 3 一等交付物:type 枚举 + reference 寻址语义定死,Epic 4 部署按
// (type, reference) 消费,无需了解构建内部。3-3 真实构建落地后经同一 StepSink.EmitArtifact
// 接口喂产物,**契约形状不变**。

// 产物类型枚举(冻结;后续新增类型只加枚举不改形状)。
const (
	// ArtifactImage 表示容器镜像(reference = repo:tag 或本地 image id)。
	ArtifactImage = "image"
	// ArtifactJar 表示 JAR 包(reference = 路径/URL)。
	ArtifactJar = "jar"
	// ArtifactDist 表示前端构建产物目录(reference = 目录/tarball 寻址)。
	ArtifactDist = "dist"
	// ArtifactArchive 表示归档包(reference = 路径)。
	ArtifactArchive = "archive"
)

// isValidArtifactType 报告 type 是否为冻结枚举之一。
func isValidArtifactType(t string) bool {
	switch t {
	case ArtifactImage, ArtifactJar, ArtifactDist, ArtifactArchive:
		return true
	default:
		return false
	}
}

// Artifact 是一次运行产出的构建产物领域模型(对齐冻结 run-detail artifacts 子 DTO)。
//
//   - Type      : image | jar | dist | archive(冻结枚举)。
//   - Name      : 产物逻辑名(如服务名)。
//   - Reference : 类型寻址引用(Epic 4 按 (Type, Reference) 消费)。
//   - SizeBytes : 字节数(未知 = 0)。
//   - Metadata  : 自由 KV(digest/path/stub 等);nil 视为空。
type Artifact struct {
	ID        string
	RunID     string
	Type      string
	Name      string
	Reference string
	SizeBytes int64
	Metadata  map[string]any
	CreatedAt time.Time
}

// encodeMetadata 把产物 metadata 序列化为可入库 JSON(nil/空 → "{}")。
func encodeMetadata(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeMetadata 把入库 JSON 反序列化为 map(空串/无效 → 空 map,绝不让坏数据致 List 失败)。
func decodeMetadata(s string) map[string]any {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// AddArtifact 持久化一条运行产物到 run_artifacts(参数化 SQL)。
// type 非法 → ErrInvalidArtifactType;run 不存在 → ErrNotFound(外键失败)。
// 由 dbStepSink.EmitArtifact / runner 报告路径调用;真实构建(3-3)经同一接口接入,契约不变。
func (s *service) AddArtifact(ctx context.Context, a Artifact) (*Artifact, error) {
	a.Type = strings.TrimSpace(a.Type)
	if !isValidArtifactType(a.Type) {
		return nil, ErrInvalidArtifactType
	}
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	metaJSON, err := encodeMetadata(a.Metadata)
	if err != nil {
		return nil, fmt.Errorf("run: encode artifact metadata: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO run_artifacts
		   (id, run_id, type, name, reference, size_bytes, metadata_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.RunID, a.Type, a.Name, a.Reference, a.SizeBytes, metaJSON,
		a.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		if isForeignKeyErr(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("run: insert artifact: %w", err)
	}
	out := a
	out.CreatedAt = a.CreatedAt.UTC()
	if out.Metadata == nil {
		out.Metadata = map[string]any{}
	}
	return &out, nil
}

// ListArtifacts 取某次运行的全部产物(按 created_at 升序,**id 定 tiebreaker**;
// 无产物 → 空切片)。created_at 为 RFC3339 秒精度,同秒插入相等 → 用 id(UUID,唯一确定)
// 破并列,保证产物先后稳定确定(跨 SQLite/MySQL 一致;MySQL 无 rowid 伪列)。run 不存在不报错
// (返回空切片);HTTP 层据 run 存在性决定 404(仿 GetLogs 语义)。参数化 SQL。
func (s *service) ListArtifacts(ctx context.Context, runID string) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, type, name, reference, size_bytes, metadata_json, created_at
		 FROM run_artifacts WHERE run_id = ? ORDER BY created_at ASC, id ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("run: load artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Artifact{}
	for rows.Next() {
		var (
			a          Artifact
			metaJSON   string
			createdStr string
		)
		if err := rows.Scan(&a.ID, &a.RunID, &a.Type, &a.Name, &a.Reference,
			&a.SizeBytes, &metaJSON, &createdStr); err != nil {
			return nil, fmt.Errorf("run: scan artifact: %w", err)
		}
		a.Metadata = decodeMetadata(metaJSON)
		if t, perr := time.Parse(time.RFC3339, createdStr); perr == nil {
			a.CreatedAt = t.UTC()
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run: iterate artifacts: %w", err)
	}
	return out, nil
}

// ArtifactFilter 是跨运行产物列表(管理面)的筛选/分页入参。零值 = 全部、第 1 页。
type ArtifactFilter struct {
	// ProjectID 过滤运行所属项目(空 = 全部项目)。
	ProjectID string
	// RunID 过滤单次运行(空 = 全部运行)。
	RunID string
	// Type 过滤产物类型;非空须为冻结枚举,否则 ErrInvalidArtifactType。
	Type string
	// Page 为 1 起页码;PageSize 钳制到 [1, maxPageSize](与 List 同口径)。
	Page    int
	PageSize int
}

// ArtifactListEntry 是跨运行列表行:产物本体 + 关联运行的项目/状态(列表展示用)。
type ArtifactListEntry struct {
	Artifact
	ProjectID   string
	ProjectName string // 冗余只读展示名(LEFT JOIN projects;缺失 = "")
	RunStatus   string // 关联运行当前状态(徽标展示)
}

// ArtifactListResult 是跨运行产物列表的分页结果(仿 ListResult)。
type ArtifactListResult struct {
	Items []ArtifactListEntry
	Page  int
	Total int
}

// ListAllArtifacts 跨运行分页列出全部产物(管理面;产物管理页数据源)。
// JOIN pipeline_runs 取运行状态,LEFT JOIN projects 取项目名;按 created_at 倒序、
// id 定 tiebreaker(最新在前,与运行列表口径一致)。type 非法 → ErrInvalidArtifactType。
func (s *service) ListAllArtifacts(ctx context.Context, f ArtifactFilter) (*ArtifactListResult, error) {
	f.Type = strings.TrimSpace(f.Type)
	if f.Type != "" && !isValidArtifactType(f.Type) {
		return nil, ErrInvalidArtifactType
	}
	page := f.Page
	if page < 1 {
		page = 1
	}
	if page > maxListPage {
		page = maxListPage
	}
	size := f.PageSize
	if size < 1 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}

	where := []string{}
	args := []any{}
	if f.RunID != "" {
		where = append(where, "a.run_id = ?")
		args = append(args, f.RunID)
	}
	if f.ProjectID != "" {
		where = append(where, "pr.project_id = ?")
		args = append(args, f.ProjectID)
	}
	if f.Type != "" {
		where = append(where, "a.type = ?")
		args = append(args, f.Type)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM run_artifacts a JOIN pipeline_runs pr ON pr.id = a.run_id`+clause,
		args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("run: count artifacts: %w", err)
	}

	offset := (page - 1) * size
	listArgs := append(append([]any{}, args...), size, offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, a.run_id, a.type, a.name, a.reference, a.size_bytes, a.metadata_json, a.created_at,
		        pr.project_id, COALESCE(p.name, ''), pr.status
		 FROM run_artifacts a
		 JOIN pipeline_runs pr ON pr.id = a.run_id
		 LEFT JOIN projects p ON p.id = pr.project_id`+clause+`
		 ORDER BY a.created_at DESC, a.id DESC
		 LIMIT ? OFFSET ?`, listArgs...)
	if err != nil {
		return nil, fmt.Errorf("run: list artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := []ArtifactListEntry{}
	for rows.Next() {
		var (
			e          ArtifactListEntry
			metaJSON   string
			createdStr string
		)
		if err := rows.Scan(&e.ID, &e.RunID, &e.Type, &e.Name, &e.Reference,
			&e.SizeBytes, &metaJSON, &createdStr,
			&e.ProjectID, &e.ProjectName, &e.RunStatus); err != nil {
			return nil, fmt.Errorf("run: scan artifact row: %w", err)
		}
		e.Metadata = decodeMetadata(metaJSON)
		if t, perr := time.Parse(time.RFC3339, createdStr); perr == nil {
			e.CreatedAt = t.UTC()
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run: iterate artifact rows: %w", err)
	}
	return &ArtifactListResult{Items: items, Page: page, Total: total}, nil
}

// artifactBlobKeys 汇总一条产物引用的全部制品库句柄:reference 本身是 64 位 hex 时计入,
// 加上 metadata JSON 文本中出现的全部 64 位 hex 串(format=files 产物的清单成员句柄只存在
// 于 metadata,不取就会漏回收)。与 retention GC 的引用集合(referencedKeys)同口径。
func artifactBlobKeys(a *Artifact) []string {
	out := make([]string, 0, 2)
	if isBlobKey(a.Reference) {
		out = append(out, strings.ToLower(a.Reference))
	}
	for _, tok := range hexTokens(encodeMetadataOrEmpty(a.Metadata)) {
		if !containsString(out, tok) {
			out = append(out, tok)
		}
	}
	return out
}

// encodeMetadataOrEmpty 把 metadata 序列化为 JSON 文本(失败/空 → "";仅句柄抽取用)。
func encodeMetadataOrEmpty(m map[string]any) string {
	b, err := encodeMetadata(m)
	if err != nil {
		return ""
	}
	if b == "{}" {
		return ""
	}
	return b
}

// isBlobKey 判断 s 是否为 64 位小写 hex(制品库句柄;与 artifactstore.validKey 同口径)。
func isBlobKey(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// containsString 报告切片是否包含指定字符串(去重用小助手)。
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// hexTokens 抽取 s 中全部 64 位 hex 串(小写化;与 retention GC 同口径:按非 hex 字符
// 切分后筛长度。metadata 体积有限,足够且零歧义)。
func hexTokens(s string) []string {
	const hexchars = "0123456789abcdefABCDEF"
	out := make([]string, 0, 2)
	start := -1
	for i := 0; i <= len(s); i++ {
		inHex := i < len(s) && strings.IndexByte(hexchars, s[i]) >= 0
		if inHex && start < 0 {
			start = i
		} else if !inHex && start >= 0 {
			if i-start == sha256.Size*2 {
				if k := strings.ToLower(s[start:i]); isBlobKey(k) {
					out = append(out, k)
				}
			}
			start = -1
		}
	}
	return out
}

// DeleteArtifact 删除一条产物记录(管理面):非终态运行 → ErrRunNotFinished;产物行不存在
// → ErrArtifactNotFound。内容寻址制品库的 blob 可被多条产物共享(去重)且 format=files 清单
// 句柄只存于 metadata,故返回「已无任何剩余行引用的句柄」(orphanKeys),由调用方对制品库
// best-effort 物理删除;引用检查用 LIKE %key%(模式在 Go 侧拼好作参数,SQLite/MySQL 通用;
// 宁可多算引用 = 宁可少删,方向安全),漏删的孤儿由每小时 GC 兜底回收。
func (s *service) DeleteArtifact(ctx context.Context, runID, artifactID string) (*Artifact, []string, error) {
	rn, err := s.Get(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	if !IsTerminal(rn.Status) {
		return nil, nil, ErrRunNotFinished
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, type, name, reference, size_bytes, metadata_json, created_at
		 FROM run_artifacts WHERE id = ? AND run_id = ?`, artifactID, runID)
	if err != nil {
		return nil, nil, fmt.Errorf("run: load artifact: %w", err)
	}
	var (
		a          Artifact
		metaJSON   string
		createdStr string
	)
	if rows.Next() {
		err = rows.Scan(&a.ID, &a.RunID, &a.Type, &a.Name, &a.Reference,
			&a.SizeBytes, &metaJSON, &createdStr)
	} else {
		err = sql.ErrNoRows
	}
	if qerr := rows.Err(); qerr != nil && err == nil {
		err = qerr
	}
	_ = rows.Close()
	if err != nil {
		return nil, nil, ErrArtifactNotFound
	}
	a.Metadata = decodeMetadata(metaJSON)
	if t, perr := time.Parse(time.RFC3339, createdStr); perr == nil {
		a.CreatedAt = t.UTC()
	}

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM run_artifacts WHERE id = ? AND run_id = ?`, artifactID, runID); err != nil {
		return nil, nil, fmt.Errorf("run: delete artifact: %w", err)
	}

	keys := artifactBlobKeys(&a)
	orphans := make([]string, 0, len(keys))
	for _, k := range keys {
		var n int
		if qerr := s.db.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM run_artifacts WHERE reference = ? OR metadata_json LIKE ?`,
			k, "%"+k+"%").Scan(&n); qerr != nil {
			continue // 引用检查失败:按「仍有引用」处理(不回收),GC 下轮兜底
		}
		if n == 0 {
			orphans = append(orphans, k)
		}
	}
	return &a, orphans, nil
}
