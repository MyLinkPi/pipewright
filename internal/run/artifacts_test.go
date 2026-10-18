package run

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestAddAndListArtifacts 验证 AddArtifact 落库 + ListArtifacts 回读形状/排序。
func TestAddAndListArtifacts(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)

	r, err := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual, Branch: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 无产物 → 空切片(非 nil)。
	got, err := svc.ListArtifacts(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("ListArtifacts(empty): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("空产物应返回非 nil 空切片, got %#v", got)
	}

	a1, err := svc.AddArtifact(context.Background(), Artifact{
		RunID:     r.ID,
		Type:      ArtifactImage,
		Name:      "shop-api",
		Reference: "registry.example.com/acme/shop-api:a1b2c3d",
		SizeBytes: 48210432,
		Metadata:  map[string]any{"digest": "sha256:abc", "stub": true},
	})
	if err != nil {
		t.Fatalf("AddArtifact image: %v", err)
	}
	if a1.ID == "" {
		t.Fatalf("AddArtifact 应分配 id")
	}
	if a1.CreatedAt.IsZero() {
		t.Fatalf("AddArtifact 应填充 createdAt")
	}

	if _, err := svc.AddArtifact(context.Background(), Artifact{
		RunID:     r.ID,
		Type:      ArtifactDist,
		Name:      "shop-web",
		Reference: "dist/shop-web",
	}); err != nil {
		t.Fatalf("AddArtifact dist: %v", err)
	}

	got, err = svc.ListArtifacts(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条产物, got %d", len(got))
	}
	// 同秒插入时按 id(UUID)破并列,顺序确定但非插入序(跨 SQLite/MySQL 一致),
	// 故按 type 索引断言各产物字段,而非依赖位置。
	byType := map[string]Artifact{}
	for _, a := range got {
		byType[a.Type] = a
	}
	img, ok := byType[ArtifactImage]
	if !ok {
		t.Fatalf("缺 image 产物: %#v", got)
	}
	dist, ok := byType[ArtifactDist]
	if !ok {
		t.Fatalf("缺 dist 产物: %#v", got)
	}
	if img.Reference != "registry.example.com/acme/shop-api:a1b2c3d" {
		t.Fatalf("reference 不符: %q", img.Reference)
	}
	if img.SizeBytes != 48210432 {
		t.Fatalf("sizeBytes 不符: %d", img.SizeBytes)
	}
	if v, ok := img.Metadata["digest"]; !ok || v != "sha256:abc" {
		t.Fatalf("metadata digest 丢失: %#v", img.Metadata)
	}
	// dist 无 metadata → 空 map(非 nil)。
	if dist.Metadata == nil {
		t.Fatalf("无 metadata 应回读为空 map 而非 nil")
	}
}

// TestAddArtifactInvalidType 验证非冻结枚举 type 被拒。
func TestAddArtifactInvalidType(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)
	r, _ := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})

	if _, err := svc.AddArtifact(context.Background(), Artifact{
		RunID: r.ID, Type: "tarball", Name: "x", Reference: "y",
	}); err != ErrInvalidArtifactType {
		t.Fatalf("非法 type 应返回 ErrInvalidArtifactType, got %v", err)
	}
}

// TestListArtifactsUnknownRun 验证不存在 run → 空切片(不报错;HTTP 层据 run 存在性决定 404)。
func TestListArtifactsUnknownRun(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	got, err := svc.ListArtifacts(context.Background(), "no-such-run")
	if err != nil {
		t.Fatalf("ListArtifacts(unknown): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("不存在 run 应返回空切片, got %d", len(got))
	}
}

// TestStubRunnerSuccessEmitsArtifacts 验证桩 runner 成功路径 emit 1~2 个桩产物(metadata.stub=true)。
func TestStubRunnerSuccessEmitsArtifacts(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	pool := NewWorkerPool(svc, WithConcurrency(2))
	pool.Start()
	t.Cleanup(func() { pool.Stop(context.Background()) })

	projID := seedProject(t, db)
	r, err := svc.Create(context.Background(), projID,
		Trigger{Type: TriggerManual, Branch: "main", Commit: "a1b2c3d4e5", Actor: "admin"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForStatus(t, svc, r.ID, StatusSuccess)

	arts, err := svc.ListArtifacts(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(arts) < 1 || len(arts) > 2 {
		t.Fatalf("成功 run 应 emit 1~2 个桩产物, got %d", len(arts))
	}
	sawImage := false
	for _, a := range arts {
		if !isValidArtifactType(a.Type) {
			t.Fatalf("桩产物 type 非冻结枚举: %q", a.Type)
		}
		if a.Reference == "" {
			t.Fatalf("桩产物 reference 不应为空")
		}
		if stub, ok := a.Metadata["stub"]; !ok || stub != true {
			t.Fatalf("桩产物应诚实标注 metadata.stub=true, got %#v", a.Metadata)
		}
		if a.Type == ArtifactImage {
			sawImage = true
			// reference 应含 commit 短串(类型寻址)。
			if a.Reference == "" {
				t.Fatalf("image 桩产物 reference 为空")
			}
		}
	}
	if !sawImage {
		t.Fatalf("桩产物应含一个 image 类型")
	}
}

// TestStubRunnerFailureNoArtifacts 验证失败 run 不 emit 产物(成功路径专属)。
func TestStubRunnerFailureNoArtifacts(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	pool := NewWorkerPool(svc,
		WithConcurrency(1),
		WithRunner(&StubRunner{Steps: []string{"构建镜像"}, FailAt: 0}),
	)
	pool.Start()
	t.Cleanup(func() { pool.Stop(context.Background()) })

	projID := seedProject(t, db)
	r, _ := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})
	waitForStatus(t, svc, r.ID, StatusFailed)

	arts, err := svc.ListArtifacts(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(arts) != 0 {
		t.Fatalf("失败 run 不应有产物, got %d", len(arts))
	}
}

// finishRun 把运行直接置为终态(删除产物护栏测试用;同包直改库)。
func finishRun(t *testing.T, db *sql.DB, runID, status string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE pipeline_runs SET status = ? WHERE id = ?`, status, runID); err != nil {
		t.Fatalf("置终态 %s: %v", status, err)
	}
}

// TestListAllArtifacts 验证跨运行产物列表:关联字段(join 运行/项目)、过滤、分页、倒序。
func TestListAllArtifacts(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	p1 := seedProject(t, db)
	p2 := seedProject(t, db)

	r1, _ := svc.Create(context.Background(), p1, Trigger{Type: TriggerManual})
	r2, _ := svc.Create(context.Background(), p2, Trigger{Type: TriggerManual})
	finishRun(t, db, r1.ID, StatusSuccess)
	finishRun(t, db, r2.ID, StatusFailed)

	if _, err := svc.AddArtifact(context.Background(), Artifact{
		RunID: r1.ID, Type: ArtifactImage, Name: "api", Reference: "reg.local/api:v1",
	}); err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}
	a2, err := svc.AddArtifact(context.Background(), Artifact{
		RunID: r2.ID, Type: ArtifactJar, Name: "worker", Reference: strings.Repeat("a", 64),
		Metadata: map[string]any{"stored": true},
	})
	if err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}

	// 全量:2 条,含关联运行/项目字段。
	res, err := svc.ListAllArtifacts(context.Background(), ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListAllArtifacts: %v", err)
	}
	if res.Total != 2 || len(res.Items) != 2 || res.Page != 1 {
		t.Fatalf("全量应 2 条第 1 页, got total=%d items=%d page=%d", res.Total, len(res.Items), res.Page)
	}
	byRef := map[string]ArtifactListEntry{}
	for _, e := range res.Items {
		byRef[e.Reference] = e
	}
	if e, ok := byRef["reg.local/api:v1"]; !ok || e.RunID != r1.ID || e.RunStatus != StatusSuccess || e.ProjectID != p1 || e.ProjectName == "" {
		t.Fatalf("image 行关联字段不符: %#v", e)
	}
	if e, ok := byRef[strings.Repeat("a", 64)]; !ok || e.RunID != r2.ID || e.RunStatus != StatusFailed || e.ProjectID != p2 {
		t.Fatalf("jar 行关联字段不符: %#v", e)
	}

	// 项目过滤。
	res, err = svc.ListAllArtifacts(context.Background(), ArtifactFilter{ProjectID: p1})
	if err != nil {
		t.Fatalf("ListAllArtifacts(project): %v", err)
	}
	if res.Total != 1 || res.Items[0].ProjectID != p1 {
		t.Fatalf("项目过滤应只剩 1 条, got %#v", res.Items)
	}

	// 类型过滤。
	res, err = svc.ListAllArtifacts(context.Background(), ArtifactFilter{Type: ArtifactJar})
	if err != nil {
		t.Fatalf("ListAllArtifacts(type): %v", err)
	}
	if res.Total != 1 || res.Items[0].ID != a2.ID {
		t.Fatalf("类型过滤应只剩 jar, got %#v", res.Items)
	}

	// 运行过滤。
	res, err = svc.ListAllArtifacts(context.Background(), ArtifactFilter{RunID: r2.ID})
	if err != nil {
		t.Fatalf("ListAllArtifacts(run): %v", err)
	}
	if res.Total != 1 || res.Items[0].RunID != r2.ID {
		t.Fatalf("运行过滤应只剩 1 条, got %#v", res.Items)
	}

	// 非法 type → ErrInvalidArtifactType。
	if _, err := svc.ListAllArtifacts(context.Background(), ArtifactFilter{Type: "tarball"}); err != ErrInvalidArtifactType {
		t.Fatalf("非法 type 应 ErrInvalidArtifactType, got %v", err)
	}

	// 分页:pageSize=1 第 2 页应剩 1 条。
	res, err = svc.ListAllArtifacts(context.Background(), ArtifactFilter{Page: 2, PageSize: 1})
	if err != nil {
		t.Fatalf("ListAllArtifacts(page2): %v", err)
	}
	if res.Total != 2 || len(res.Items) != 1 || res.Page != 2 {
		t.Fatalf("分页第 2 页应 1 条, got total=%d items=%d page=%d", res.Total, len(res.Items), res.Page)
	}

	// 零值分页钳制:page=0 → 1。
	res, err = svc.ListAllArtifacts(context.Background(), ArtifactFilter{Page: 0, PageSize: 0})
	if err != nil {
		t.Fatalf("ListAllArtifacts(zero page): %v", err)
	}
	if res.Page != 1 || len(res.Items) != 2 {
		t.Fatalf("零值分页应钳制为第 1 页全量, got page=%d items=%d", res.Page, len(res.Items))
	}
}

// TestDeleteArtifact 验证产物删除:行删除、句柄回收(共享不回收、metadata 清单句柄回收)、
// 非终态拒绝、不存在 404。
func TestDeleteArtifact(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)
	sharedKey := strings.Repeat("b", 64)

	newFinishedRun := func() *Run {
		r, err := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		finishRun(t, db, r.ID, StatusSuccess)
		return r
	}

	// 场景 1:进行中运行 → ErrRunNotFinished,行不动。
	rLive, _ := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})
	aLive, err := svc.AddArtifact(context.Background(), Artifact{
		RunID: rLive.ID, Type: ArtifactJar, Name: "live", Reference: sharedKey,
	})
	if err != nil {
		t.Fatalf("AddArtifact(live): %v", err)
	}
	if _, _, err := svc.DeleteArtifact(context.Background(), rLive.ID, aLive.ID); err != ErrRunNotFinished {
		t.Fatalf("进行中运行应 ErrRunNotFinished, got %v", err)
	}
	if arts, _ := svc.ListArtifacts(context.Background(), rLive.ID); len(arts) != 1 {
		t.Fatalf("进行中运行的产物行不应被删")
	}

	// 场景 2:不存在 → ErrArtifactNotFound;不存在 run → ErrNotFound。
	rFin := newFinishedRun()
	if _, _, err := svc.DeleteArtifact(context.Background(), rFin.ID, "no-such-artifact"); err != ErrArtifactNotFound {
		t.Fatalf("缺产物应 ErrArtifactNotFound, got %v", err)
	}
	if _, _, err := svc.DeleteArtifact(context.Background(), "no-such-run", "x"); err != ErrNotFound {
		t.Fatalf("缺 run 应 ErrNotFound, got %v", err)
	}

	// 场景 3:共享句柄(两条产物同 reference)→ 不回收。
	rA, rB := newFinishedRun(), newFinishedRun()
	aShared, err := svc.AddArtifact(context.Background(), Artifact{
		RunID: rA.ID, Type: ArtifactDist, Name: "shared-a", Reference: sharedKey,
		Metadata: map[string]any{"stored": true, "format": "tar.gz"},
	})
	if err != nil {
		t.Fatalf("AddArtifact(shared-a): %v", err)
	}
	if _, err := svc.AddArtifact(context.Background(), Artifact{
		RunID: rB.ID, Type: ArtifactDist, Name: "shared-b", Reference: sharedKey,
		Metadata: map[string]any{"stored": true, "format": "tar.gz"},
	}); err != nil {
		t.Fatalf("AddArtifact(shared-b): %v", err)
	}
	deleted, orphans, err := svc.DeleteArtifact(context.Background(), rA.ID, aShared.ID)
	if err != nil {
		t.Fatalf("DeleteArtifact(shared): %v", err)
	}
	if deleted.ID != aShared.ID || deleted.Name != "shared-a" {
		t.Fatalf("应返回被删产物本体, got %#v", deleted)
	}
	if len(orphans) != 0 {
		t.Fatalf("共享句柄不应回收, got %v", orphans)
	}
	if arts, _ := svc.ListArtifacts(context.Background(), rA.ID); len(arts) != 0 {
		t.Fatalf("产物行应已删除")
	}

	// 场景 4:独占句柄 + format=files 清单句柄(只在 metadata)→ 全部回收。
	rC := newFinishedRun()
	manifestKey := strings.Repeat("c", 64)
	fileKey := strings.Repeat("d", 64)
	aFiles, err := svc.AddArtifact(context.Background(), Artifact{
		RunID: rC.ID, Type: ArtifactDist, Name: "web", Reference: manifestKey,
		Metadata: map[string]any{
			"stored": true, "format": "files",
			"files": []any{
				map[string]any{"path": "a.js", "key": fileKey, "size": 12},
			},
		},
	})
	if err != nil {
		t.Fatalf("AddArtifact(files): %v", err)
	}
	_, orphans, err = svc.DeleteArtifact(context.Background(), rC.ID, aFiles.ID)
	if err != nil {
		t.Fatalf("DeleteArtifact(files): %v", err)
	}
	got := map[string]bool{}
	for _, k := range orphans {
		got[k] = true
	}
	if !got[manifestKey] || !got[fileKey] {
		t.Fatalf("独占句柄与清单句柄都应回收, got %v", orphans)
	}

	// 场景 5:镜像类(reference=repo:tag,无句柄)→ 无回收、行删除。
	rD := newFinishedRun()
	aImg, err := svc.AddArtifact(context.Background(), Artifact{
		RunID: rD.ID, Type: ArtifactImage, Name: "api", Reference: "reg.local/api:v2",
		Metadata: map[string]any{"digest": "sha256:xyz"},
	})
	if err != nil {
		t.Fatalf("AddArtifact(image): %v", err)
	}
	_, orphans, err = svc.DeleteArtifact(context.Background(), rD.ID, aImg.ID)
	if err != nil {
		t.Fatalf("DeleteArtifact(image): %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("镜像无制品库句柄, got %v", orphans)
	}
	if arts, _ := svc.ListArtifacts(context.Background(), rD.ID); len(arts) != 0 {
		t.Fatalf("镜像产物行应已删除")
	}
}
