package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// testMasterKey 返回确定性测试用 master key。
func testMasterKey() *[32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 11)
	}
	return &k
}

// settingsTestDB 打开临时 SQLite(含全部迁移),返回 *sql.DB 与库文件路径(供整库 dump)。
func settingsTestDB(t *testing.T) (*sql.DB, string) {
	return storetest.OpenDBWithPath(t)
}

// seedSettingsProject 插入一个最小凭据 + 项目,返回 project id。
func seedSettingsProject(t *testing.T, db *sql.DB) string {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	credID := uuid.NewString()
	_, err := db.Exec(
		`INSERT INTO credentials (id, name, type, scope, ciphertext, masked_value, created_at, updated_at)
		 VALUES (?, 'c', 'git_token', '', X'00', 'm', ?, ?)`,
		credID, now, now,
	)
	if err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	projID := uuid.NewString()
	_, err = db.Exec(
		`INSERT INTO projects (id, name, repo_url, default_branch, credential_id, created_at, updated_at)
		 VALUES (?, 'p', 'https://example.com/p.git', 'main', ?, ?, ?)`,
		projID, credID, now, now,
	)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return projID
}

// seedRealCred 经 vault 真实创建一个含明文 secret 的 git_token 凭据,返回 (id, 明文)。
func seedRealCred(t *testing.T, v vault.Vault, secret string) string {
	t.Helper()
	c, err := v.Create(vault.CreateInput{Name: "tok", Type: vault.TypeGitToken, Secret: secret})
	if err != nil {
		t.Fatalf("vault.Create: %v", err)
	}
	return c.ID
}

func newSettingsSvc(t *testing.T) (SettingsService, vault.Vault, *sql.DB, string, string) {
	t.Helper()
	db, dbPath := settingsTestDB(t)
	v := vault.New(db, testMasterKey())
	svc := NewSettingsService(db, v)
	projID := seedSettingsProject(t, db)
	return svc, v, db, dbPath, projID
}

func TestSettingsGetLazyDefault(t *testing.T) {
	svc, _, _, _, projID := newSettingsSvc(t)
	st, err := svc.Get(context.Background(), projID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.Build.Model != BuildModelDockerfile {
		t.Fatalf("默认构建模型应为 dockerfile, got %q", st.Build.Model)
	}
	if st.Build.DockerfilePath != defaultDockerfilePath {
		t.Fatalf("默认 Dockerfile 路径 = %q", st.Build.DockerfilePath)
	}
	if st.Build.ArtifactType != ArtifactImage {
		t.Fatalf("默认产物应为 image, got %q", st.Build.ArtifactType)
	}
	if st.Build.Vars == nil || len(st.Build.Vars) != 0 {
		t.Fatalf("默认构建变量应为空切片, got %+v", st.Build.Vars)
	}

	// 二次 Get 幂等。
	st2, err := svc.Get(context.Background(), projID)
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if !st2.UpdatedAt.Equal(st.UpdatedAt) {
		t.Fatalf("重复 Get 不应改 updatedAt: %v != %v", st2.UpdatedAt, st.UpdatedAt)
	}
}

func TestSettingsGetProjectNotFound(t *testing.T) {
	svc, _, _, _, _ := newSettingsSvc(t)
	_, err := svc.Get(context.Background(), uuid.NewString())
	if err != ErrProjectNotFound {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}

func TestSettingsSaveBuildModelToggleRoundTrip(t *testing.T) {
	svc, _, _, _, projID := newSettingsSvc(t)
	ctx := context.Background()

	// 模型 B + 工具链 + 产物 jar + 明文变量 + 缓存。
	in := SettingsInput{
		Build: BuildConfig{
			Model:        BuildModelToolchain,
			Toolchain:    Toolchain{Language: "node", Version: "22"},
			ArtifactType: ArtifactJAR,
			Vars: []BuildVar{
				{Key: "NODE_ENV", Secret: false, Value: "production"},
			},
			Cache: Cache{Enabled: true, Paths: []string{"node_modules", ".npm"}},
		},
	}
	st, err := svc.Save(ctx, projID, in)
	if err != nil {
		t.Fatalf("Save B: %v", err)
	}
	if st.Build.Model != BuildModelToolchain || st.Build.Toolchain.Version != "22" {
		t.Fatalf("模型 B 往返失败: %+v", st.Build)
	}
	if st.Build.ArtifactType != ArtifactJAR {
		t.Fatalf("产物应为 jar, got %q", st.Build.ArtifactType)
	}
	if len(st.Build.Vars) != 1 || st.Build.Vars[0].Value != "production" || st.Build.Vars[0].ID == "" {
		t.Fatalf("明文变量往返失败(应补 id): %+v", st.Build.Vars)
	}
	if !st.Build.Cache.Enabled || len(st.Build.Cache.Paths) != 2 {
		t.Fatalf("缓存往返失败: %+v", st.Build.Cache)
	}

	// 切回模型 A:DockerfilePath 应补默认。
	st2, err := svc.Save(ctx, projID, SettingsInput{Build: BuildConfig{Model: BuildModelDockerfile, ArtifactType: ArtifactImage}})
	if err != nil {
		t.Fatalf("Save A: %v", err)
	}
	if st2.Build.Model != BuildModelDockerfile || st2.Build.DockerfilePath != defaultDockerfilePath {
		t.Fatalf("模型 A 切换失败: %+v", st2.Build)
	}
}

// TestSettingsBuildContextRoundTrip 锁构建上下文目录(build.context)的保存/回读往返:
// 此前 normalizeBuild 不拷贝、storedBuild 无该字段,保存即丢(monorepo 子目录构建失效)。
func TestSettingsBuildContextRoundTrip(t *testing.T) {
	svc, _, _, _, projID := newSettingsSvc(t)
	ctx := context.Background()

	// 含外围空白的 context 应 trim 后持久化。
	_, err := svc.Save(ctx, projID, SettingsInput{Build: BuildConfig{
		Model:          BuildModelDockerfile,
		DockerfilePath: "backend/Dockerfile",
		Context:        " backend ",
		ArtifactType:   ArtifactImage,
	}})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.Build.Context != "backend" {
		t.Fatalf("context 往返失败: got %q, want %q", st.Build.Context, "backend")
	}

	// 清空 context 回退为仓库根(空串语义)。
	if _, err := svc.Save(ctx, projID, SettingsInput{Build: BuildConfig{
		Model: BuildModelDockerfile, ArtifactType: ArtifactImage,
	}}); err != nil {
		t.Fatalf("Save 清空: %v", err)
	}
	st2, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if st2.Build.Context != "" {
		t.Fatalf("清空后 context = %q, want 空串", st2.Build.Context)
	}
}

// TestSettingsSaveDuplicateIDsRejected 锁步骤/变量 id 的唯一性校验:
// 此前只补空 id 不查重,同 id 记录会静默共存(回读/引用语义错乱)。
func TestSettingsSaveDuplicateIDsRejected(t *testing.T) {
	svc, _, _, _, projID := newSettingsSvc(t)
	ctx := context.Background()

	// 变量 id 重复 → ErrInvalidVar。
	_, err := svc.Save(ctx, projID, SettingsInput{Build: BuildConfig{
		Vars: []BuildVar{
			{ID: "vdup", Key: "A", Value: "1"},
			{ID: "vdup", Key: "B", Value: "2"},
		},
	}})
	if !errors.Is(err, ErrInvalidVar) {
		t.Fatalf("变量 id 重复 err = %v, want ErrInvalidVar", err)
	}

	// 步骤 id 重复 → ErrInvalidStep。
	_, err = svc.Save(ctx, projID, SettingsInput{
		Steps: []PipelineStep{
			{ID: "sdup", Name: "单测", Image: "golang:1.23", Commands: []string{"go test ./..."}},
			{ID: "sdup", Name: "构建", Image: "golang:1.23", Commands: []string{"go build ./..."}},
		},
	})
	if !errors.Is(err, ErrInvalidStep) {
		t.Fatalf("步骤 id 重复 err = %v, want ErrInvalidStep", err)
	}
}

func TestSettingsSaveInvalidBuildModel(t *testing.T) {
	svc, _, _, _, projID := newSettingsSvc(t)
	_, err := svc.Save(context.Background(), projID, SettingsInput{Build: BuildConfig{Model: "bogus"}})
	if err == nil || !strings.Contains(err.Error(), "invalid build") {
		t.Fatalf("err = %v, want ErrInvalidBuild", err)
	}
}

func TestSettingsSaveInvalidArtifact(t *testing.T) {
	svc, _, _, _, projID := newSettingsSvc(t)
	_, err := svc.Save(context.Background(), projID, SettingsInput{Build: BuildConfig{Model: BuildModelDockerfile, ArtifactType: "bogus"}})
	if err == nil || !strings.Contains(err.Error(), "invalid build") {
		t.Fatalf("err = %v, want ErrInvalidBuild", err)
	}
}

func TestSettingsSaveDuplicateVarKey(t *testing.T) {
	svc, _, _, _, projID := newSettingsSvc(t)
	_, err := svc.Save(context.Background(), projID, SettingsInput{Build: BuildConfig{
		Vars: []BuildVar{
			{Key: "A", Value: "1"},
			{Key: "A", Value: "2"},
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("err = %v, want ErrInvalidVar duplicate", err)
	}
}

func TestSettingsSaveEmptyVarKey(t *testing.T) {
	svc, _, _, _, projID := newSettingsSvc(t)
	_, err := svc.Save(context.Background(), projID, SettingsInput{Build: BuildConfig{
		Vars: []BuildVar{{Key: "  ", Value: "x"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "key must not be empty") {
		t.Fatalf("err = %v, want ErrInvalidVar empty", err)
	}
}

func TestSettingsSecretVarRoundTripMasked(t *testing.T) {
	svc, v, _, _, projID := newSettingsSvc(t)
	const plaintext = "ghp_LEAKMARKER_secret_value_zzz"
	credID := seedRealCred(t, v, plaintext)

	st, err := svc.Save(context.Background(), projID, SettingsInput{Build: BuildConfig{
		Vars: []BuildVar{
			{Key: "NPM_TOKEN", Secret: true, CredentialID: credID, Value: plaintext}, // value 应被丢弃
		},
	}})
	if err != nil {
		t.Fatalf("Save secret: %v", err)
	}
	if len(st.Build.Vars) != 1 {
		t.Fatalf("变量数 = %d", len(st.Build.Vars))
	}
	got := st.Build.Vars[0]
	if !got.Secret || got.CredentialID != credID {
		t.Fatalf("secret 引用未保留: %+v", got)
	}
	if got.Value != "" {
		t.Fatalf("secret 项绝不应回显明文 value: %q", got.Value)
	}
	if got.MaskedValue == "" || strings.Contains(got.MaskedValue, "LEAKMARKER") {
		t.Fatalf("应回掩码且不含明文: %q", got.MaskedValue)
	}
}

func TestSettingsSecretVarCredentialNotFound(t *testing.T) {
	svc, _, _, _, projID := newSettingsSvc(t)
	_, err := svc.Save(context.Background(), projID, SettingsInput{Build: BuildConfig{
		Vars: []BuildVar{{Key: "X", Secret: true, CredentialID: uuid.NewString()}},
	}})
	if err == nil || !strings.Contains(err.Error(), "referenced credential not found") {
		t.Fatalf("err = %v, want ErrCredentialNotFound", err)
	}
}

func TestSettingsSecretVarVaultUnconfigured(t *testing.T) {
	db, _ := settingsTestDB(t)
	projID := seedSettingsProject(t, db)
	svc := NewSettingsService(db, vault.New(db, nil)) // 未配置 master key
	_, err := svc.Save(context.Background(), projID, SettingsInput{Build: BuildConfig{
		Vars: []BuildVar{{Key: "X", Secret: true, CredentialID: uuid.NewString()}},
	}})
	if err != ErrSettingsVaultUnconfigured {
		t.Fatalf("err = %v, want ErrSettingsVaultUnconfigured", err)
	}
	// 无 secret 引用时,即便未配置 vault 也应能保存。
	if _, err := svc.Save(context.Background(), projID, SettingsInput{Build: BuildConfig{Model: BuildModelDockerfile}}); err != nil {
		t.Fatalf("无 secret 保存不应需 vault: %v", err)
	}
}

// TestSettingsSecAfterDBNoPlaintext 断言:存含 secret 引用的 settings 后,整库二进制不含任何明文 secret(AC-SEC)。
func TestSettingsSecAfterDBNoPlaintext(t *testing.T) {
	svc, v, db, dbPath, projID := newSettingsSvc(t)
	const buildSecret = "ghp_BUILDLEAK_marker_value_xyz9"
	buildCred := seedRealCred(t, v, buildSecret)

	_, err := svc.Save(context.Background(), projID, SettingsInput{
		Build: BuildConfig{
			Vars: []BuildVar{{Key: "NPM_TOKEN", Secret: true, CredentialID: buildCred, Value: buildSecret}},
		},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)

	// pipeline_settings 表的 JSON 列裸读:断言不含明文。
	var buildJSON string
	if err := db.QueryRow(`SELECT build_json FROM pipeline_settings WHERE project_id = ?`, projID).
		Scan(&buildJSON); err != nil {
		t.Fatalf("read settings row: %v", err)
	}
	for _, blob := range []string{buildJSON} {
		if strings.Contains(blob, buildSecret) {
			t.Fatalf("pipeline_settings JSON 含明文 secret!: %s", blob)
		}
		if strings.Contains(blob, "LEAK") {
			t.Fatalf("pipeline_settings JSON 含泄漏标记!: %s", blob)
		}
	}

	// 整库文件(含 WAL/SHM)二进制也不含明文。
	if dbPath != "" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			raw := readSettingsFileMaybe(t, dbPath+suffix)
			if strings.Contains(string(raw), buildSecret) {
				t.Fatalf("整库文件 %s 含明文 secret!", dbPath+suffix)
			}
		}
	}
}

func TestSettingsDeleteProjectCascades(t *testing.T) {
	svc, _, db, _, projID := newSettingsSvc(t)
	if _, err := svc.Get(context.Background(), projID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM projects WHERE id = ?`, projID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM pipeline_settings WHERE project_id = ?`, projID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("删项目应级联删 settings, 残留 %d 行", n)
	}
}

func readSettingsFileMaybe(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// ---- 流水线即代码变量(声明同步 + 值保存)----

func TestSyncPacVarsMergeAndSave(t *testing.T) {
	svc, v, db, dbPath, projID := newSettingsSvc(t)
	ctx := context.Background()
	credID := seedRealCred(t, v, "s3cret-conn")

	// 首次同步:两条声明。
	st, err := svc.SyncPacVars(ctx, projID, []PacVarDef{
		{Name: "DB_CONN", Description: "数据库连接串", Required: true, Secret: true},
		{Name: "BASE_DOMAIN", Description: "服务注册基域", Default: "internal.example.com"},
	}, "main", ".pipewright.yml")
	if err != nil {
		t.Fatalf("SyncPacVars: %v", err)
	}
	if len(st.PacVars.Defs) != 2 || len(st.PacVars.Values) != 0 {
		t.Fatalf("首次同步应 2 声明 0 值: %+v", st.PacVars)
	}

	// 填值:secret 给 credentialId,明文给 value。
	st, err = svc.Save(ctx, projID, SettingsInput{PacVars: &PacVarsInput{Values: []BuildVar{
		{Key: "DB_CONN", Secret: true, CredentialID: credID},
		{Key: "BASE_DOMAIN", Value: "corp.example.com"},
	}}})
	if err != nil {
		t.Fatalf("Save pac vars: %v", err)
	}
	if len(st.PacVars.Values) != 2 {
		t.Fatalf("应保存 2 条值: %+v", st.PacVars.Values)
	}
	for _, val := range st.PacVars.Values {
		if val.Key == "DB_CONN" && (val.MaskedValue == "" || val.Value != "") {
			t.Fatalf("secret 值应只有掩码无明文: %+v", val)
		}
	}

	// 再同步:去掉 BASE_DOMAIN、新增 SUB_DOMAIN → 孤儿值删、新声明未填、DB_CONN 值保留。
	st, err = svc.SyncPacVars(ctx, projID, []PacVarDef{
		{Name: "DB_CONN", Required: true, Secret: true},
		{Name: "SUB_DOMAIN"},
	}, "release", "ci/p.yml")
	if err != nil {
		t.Fatalf("re-SyncPacVars: %v", err)
	}
	if len(st.PacVars.Defs) != 2 || len(st.PacVars.Values) != 1 || st.PacVars.Values[0].Key != "DB_CONN" {
		t.Fatalf("再同步应保留 DB_CONN 值并清理孤儿: %+v", st.PacVars)
	}
	if st.PacVars.Ref != "release" || st.PacVars.File != "ci/p.yml" || st.PacVars.SyncedAt.IsZero() {
		t.Fatalf("同步来源元信息应更新: %+v", st.PacVars)
	}

	// PacVars 缺省(nil)保存不抹值:只改缓存,值仍在。
	st, err = svc.Save(ctx, projID, SettingsInput{Build: st.Build})
	if err != nil {
		t.Fatalf("Save without pacVars: %v", err)
	}
	if len(st.PacVars.Values) != 1 || st.PacVars.Values[0].Key != "DB_CONN" {
		t.Fatalf("PacVars nil 应保留现值: %+v", st.PacVars.Values)
	}

	// 铁律:落库 JSON 绝无 secret 明文。
	dump, _ := os.ReadFile(dbPath)
	if strings.Contains(string(dump), "s3cret-conn") {
		t.Fatalf("pac_vars_json 不应含 secret 明文")
	}
	_ = db
}

func TestSavePacVarValuesValidation(t *testing.T) {
	svc, v, _, _, projID := newSettingsSvc(t)
	ctx := context.Background()
	credID := seedRealCred(t, v, "plain")

	if _, err := svc.SyncPacVars(ctx, projID, []PacVarDef{
		{Name: "SECRET_VAR", Secret: true},
		{Name: "PLAIN_VAR"},
	}, "main", ".pipewright.yml"); err != nil {
		t.Fatalf("SyncPacVars: %v", err)
	}

	cases := []struct {
		name   string
		values []BuildVar
	}{
		{"未声明变量", []BuildVar{{Key: "GHOST", Value: "x"}}},
		{"secret缺credentialId", []BuildVar{{Key: "SECRET_VAR", Secret: true, Value: "x"}}},
		{"secret凭据不存在", []BuildVar{{Key: "SECRET_VAR", Secret: true, CredentialID: uuid.NewString()}}},
		{"重复key", []BuildVar{{Key: "PLAIN_VAR", Value: "a"}, {Key: "PLAIN_VAR", Value: "b"}}},
	}
	for _, tc := range cases {
		_, err := svc.Save(ctx, projID, SettingsInput{PacVars: &PacVarsInput{Values: tc.values}})
		if !errors.Is(err, ErrInvalidVar) && !errors.Is(err, ErrCredentialNotFound) {
			t.Fatalf("%s: 应报 ErrInvalidVar/ErrCredentialNotFound,得到 %v", tc.name, err)
		}
	}

	// secret 标志以声明为准:明文声明误传 credentialId → 只存明文值,不落引用。
	st, err := svc.Save(ctx, projID, SettingsInput{PacVars: &PacVarsInput{Values: []BuildVar{
		{Key: "PLAIN_VAR", CredentialID: credID, Value: "hello"},
	}}})
	if err != nil {
		t.Fatalf("Save plain var: %v", err)
	}
	if len(st.PacVars.Values) != 1 || st.PacVars.Values[0].Value != "hello" || st.PacVars.Values[0].CredentialID != "" {
		t.Fatalf("明文声明应只存 value: %+v", st.PacVars.Values[0])
	}
}

func TestSyncPacVarsSecretFlipDropsValue(t *testing.T) {
	svc, _, _, _, projID := newSettingsSvc(t)
	ctx := context.Background()

	if _, err := svc.SyncPacVars(ctx, projID, []PacVarDef{{Name: "DOMAIN"}}, "main", ".pipewright.yml"); err != nil {
		t.Fatalf("SyncPacVars: %v", err)
	}
	if _, err := svc.Save(ctx, projID, SettingsInput{PacVars: &PacVarsInput{Values: []BuildVar{
		{Key: "DOMAIN", Value: "x.example.com"},
	}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 声明翻转为 secret:原明文值不再兼容,应被丢弃(防明文按 secret 语义误用)。
	st, err := svc.SyncPacVars(ctx, projID, []PacVarDef{{Name: "DOMAIN", Secret: true}}, "main", ".pipewright.yml")
	if err != nil {
		t.Fatalf("re-SyncPacVars: %v", err)
	}
	if len(st.PacVars.Values) != 0 {
		t.Fatalf("secret 翻转后应丢弃明文值: %+v", st.PacVars.Values)
	}
}
