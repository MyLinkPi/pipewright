// Package pushsource 是「被推送的产物/镜像」源的修订领域(集群推送接收侧数据)。
//
// 每次对等推送成功落一行 pushed_revisions;file 条目真字节在制品库(内容寻址,
// 天然去重),image 条目的 docker save 归档同为制品库 blob,由接收侧导入本机镜像面后
// 记录本地可用引用(Items[].ImageRef)。
//
// 运行期选择约定(随触发参数携带,恢复运行自动继承):
//
//	pw.push.revision = <修订标签>(空 = 最新)
//	pw.push.items    = <条目名逗号列表>(空 = 全部条目)
package pushsource

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 触发参数保留键(运行时选择修订/条目;见包注释)。
const (
	ParamRevision = "pw.push.revision"
	ParamItems    = "pw.push.items"
)

// 条目类型枚举。
const (
	KindFile  = "file"
	KindImage = "image"
)

// 领域错误。
var (
	// ErrNotFound 表示修订不存在。
	ErrNotFound = errors.New("pushsource: not found")
	// ErrInvalidItem 表示条目字段非法(kind 枚举外/哈希为空等)。
	ErrInvalidItem = errors.New("pushsource: invalid item")
)

// Item 是修订里的一个条目。
type Item struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // file | image
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	DestName string `json:"destName,omitempty"`
	// ImageRef 是接收端导入后的本地可用引用(内置 registry tag);file 条目为空。
	ImageRef string `json:"imageRef,omitempty"`
	// Digest 是镜像内容 digest(sha256:…):跨推送去重的稳定键(归档哈希每次导出可能不同)。
	Digest string `json:"digest,omitempty"`
}

// Revision 是一次推送修订。
type Revision struct {
	ID           string
	ProjectID    string
	Label        string
	OriginNodeID string
	OriginRunID  string
	Items        []Item
	CreatedAt    time.Time
}

// Store 定义修订存取接口。
type Store interface {
	// Create 落一行修订(items 序列化为 JSON)。
	Create(ctx context.Context, rev Revision) (*Revision, error)
	// Get 按 id 取修订。
	Get(ctx context.Context, id string) (*Revision, error)
	// Latest 取项目最新修订(按 created_at,id 兜底);无修订 → ErrNotFound。
	Latest(ctx context.Context, projectID string) (*Revision, error)
	// List 按时间倒序列项目修订(limit<=0 默认 20,上限 100)。
	List(ctx context.Context, projectID string, limit int) ([]Revision, error)
}

type store struct {
	db *sql.DB
}

// New 构造修订存储。
func New(db *sql.DB) Store { return &store{db: db} }

func (s *store) Create(ctx context.Context, rev Revision) (*Revision, error) {
	if rev.ID == "" {
		rev.ID = uuid.NewString()
	}
	for _, it := range rev.Items {
		if (it.Kind != KindFile && it.Kind != KindImage) || it.SHA256 == "" || it.Name == "" {
			return nil, fmt.Errorf("%w: %q", ErrInvalidItem, it.Name)
		}
	}
	itemsJSON, err := json.Marshal(rev.Items)
	if err != nil {
		return nil, fmt.Errorf("pushsource: marshal items: %w", err)
	}
	if rev.CreatedAt.IsZero() {
		rev.CreatedAt = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO pushed_revisions (id, project_id, label, origin_node_id, origin_run_id, items, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rev.ID, rev.ProjectID, rev.Label, rev.OriginNodeID, rev.OriginRunID,
		string(itemsJSON), rev.CreatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("pushsource: insert: %w", err)
	}
	return &rev, nil
}

const revisionCols = `id, project_id, label, origin_node_id, origin_run_id, items, created_at`

func scanRevision(sc func(dest ...any) error) (*Revision, error) {
	var rev Revision
	var itemsJSON, createdStr string
	if err := sc(&rev.ID, &rev.ProjectID, &rev.Label, &rev.OriginNodeID, &rev.OriginRunID,
		&itemsJSON, &createdStr); err != nil {
		return nil, err
	}
	rev.Items = []Item{}
	if strings.TrimSpace(itemsJSON) != "" {
		_ = json.Unmarshal([]byte(itemsJSON), &rev.Items)
	}
	created, err := time.Parse(time.RFC3339, createdStr)
	if err != nil {
		return nil, fmt.Errorf("pushsource: parse created_at: %w", err)
	}
	rev.CreatedAt = created
	return &rev, nil
}

func (s *store) Get(ctx context.Context, id string) (*Revision, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+revisionCols+` FROM pushed_revisions WHERE id = ?`, id)
	rev, err := scanRevision(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rev, err
}

func (s *store) Latest(ctx context.Context, projectID string) (*Revision, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+revisionCols+` FROM pushed_revisions WHERE project_id = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, projectID)
	rev, err := scanRevision(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rev, err
}

func (s *store) List(ctx context.Context, projectID string, limit int) ([]Revision, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+revisionCols+` FROM pushed_revisions WHERE project_id = ?
		 ORDER BY created_at DESC, id DESC LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("pushsource: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Revision{}
	for rows.Next() {
		rev, err := scanRevision(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *rev)
	}
	return out, rows.Err()
}

// Resolve 解析一次运行的取用意图:revisionLabel 空 → 最新;itemNames 空 → 全部条目。
// itemNames 中出现修订里不存在的名字 → ErrNotFound(诚实失败,不静默忽略)。
func Resolve(ctx context.Context, st Store, projectID, revisionLabel string, itemNames []string) (*Revision, []Item, error) {
	var rev *Revision
	var err error
	if strings.TrimSpace(revisionLabel) == "" {
		rev, err = st.Latest(ctx, projectID)
	} else {
		revs, lerr := st.List(ctx, projectID, 100)
		if lerr != nil {
			return nil, nil, lerr
		}
		for i := range revs {
			if revs[i].Label == strings.TrimSpace(revisionLabel) {
				rev = &revs[i]
				break
			}
		}
		if rev == nil {
			return nil, nil, ErrNotFound
		}
		err = nil
	}
	if err != nil {
		return nil, nil, err
	}
	if len(itemNames) == 0 {
		return rev, rev.Items, nil
	}
	want := make(map[string]struct{}, len(itemNames))
	for _, n := range itemNames {
		want[strings.TrimSpace(n)] = struct{}{}
	}
	items := make([]Item, 0, len(want))
	for _, it := range rev.Items {
		if _, ok := want[it.Name]; ok {
			items = append(items, it)
			delete(want, it.Name)
		}
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for n := range want {
			missing = append(missing, n)
		}
		return nil, nil, fmt.Errorf("%w: items %v not in revision %s", ErrNotFound, missing, rev.Label)
	}
	return rev, items, nil
}
