package backup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
)

// newTestStore 打开临时 SQLite 库(应用全部内嵌迁移,含 platform_backup_*)。
func newTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "pipewright.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

func newTestService(t *testing.T, dir string, st *store.Store) *Service {
	t.Helper()
	return New(st.DB, Options{
		Driver:  "sqlite",
		DataDir: dir,
		DBPath:  filepath.Join(dir, "pipewright.db"),
	})
}

// waitForBackup 轮询等备份终态(异步执行)。
func waitForBackup(t *testing.T, svc *Service, id string) Record {
	t.Helper()
	deadline := waitFor(50, func() bool {
		rec, err := svc.Get(context.Background(), id)
		return err == nil && (rec.Status == "completed" || rec.Status == "failed")
	})
	rec, err := svc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if rec.Status != "completed" {
		t.Fatalf("backup did not complete: status=%s error=%s", rec.Status, rec.Error)
	}
	_ = deadline
	return *rec
}

func waitFor(steps int, cond func() bool) bool {
	for i := 0; i < steps; i++ {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}

func TestBackupRoundtripAndRestore(t *testing.T) {
	st, dir := newTestStore(t)
	svc := newTestService(t, dir, st)

	// 造一点业务数据(自定义表 + 数据,验证快照内容完整)。
	if _, err := st.DB.Exec(`CREATE TABLE marker (id INTEGER PRIMARY KEY, note TEXT)`); err != nil {
		t.Fatalf("create marker table: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO marker (note) VALUES ('hello-backup')`); err != nil {
		t.Fatalf("insert marker: %v", err)
	}

	rec0, err := svc.CreateBackup(context.Background(), CreateInput{Kind: "manual"})
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	rec := waitForBackup(t, svc, rec0.ID)

	if rec.SizeBytes <= 0 || len(rec.Sha256) != 64 {
		t.Fatalf("unexpected record: %+v", rec)
	}
	archive := svc.ArchivePath(&rec)
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("archive missing: %v", err)
	}

	// 校验器应通过,且内容面正确。
	m, err := ValidateArchive(archive)
	if err != nil {
		t.Fatalf("validate archive: %v", err)
	}
	if !m.Contains.DB || m.DBDriver != "sqlite" {
		t.Fatalf("unexpected manifest: %+v", m)
	}

	// 破坏归档 → 校验必须失败。
	broken := archive + ".broken"
	raw, _ := os.ReadFile(archive)
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(broken, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateArchive(broken); err == nil {
		t.Fatal("corrupted archive passed validation")
	}

	// 恢复到「另一台机器」:新数据目录 + 从备份拷来的库文件名一致。
	// 预放一个「现存旧库」,验证安全备份(无旧库 = 全新安装,安全备份为空亦是合法)。
	restoreDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(restoreDir, "pipewright.db"), []byte("old db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(archive, filepath.Join(restoreDir, filepath.Base(archive)), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := ApplyRestore(filepath.Join(restoreDir, filepath.Base(archive)), RestoreOptions{
		Driver: "sqlite",
		DataDir: restoreDir,
		DBPath: filepath.Join(restoreDir, "pipewright.db"),
	})
	if err != nil {
		t.Fatalf("apply restore: %v", err)
	}
	if !rep.AppliedDB {
		t.Fatalf("db not applied: %+v", rep)
	}
	// 安全备份应记录原库路径。
	if _, err := os.Stat(rep.DBSafetyPath); err != nil {
		t.Fatalf("safety backup missing: %v", err)
	}

	// 打开恢复后的库验证数据。
	st2, err := store.Open(filepath.Join(restoreDir, "pipewright.db"))
	if err != nil {
		t.Fatalf("open restored store: %v", err)
	}
	defer func() { _ = st2.Close() }()
	var note string
	if err := st2.DB.QueryRow(`SELECT note FROM marker WHERE id = 1`).Scan(&note); err != nil {
		t.Fatalf("restored data missing: %v", err)
	}
	if note != "hello-backup" {
		t.Fatalf("restored data mismatch: %q", note)
	}
	// 迁移表随快照带走(不再重复应用)。
	var n int
	if err := st2.DB.QueryRow(`SELECT COUNT(1) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("schema_migrations missing in restored db: %v", err)
	}
}

func TestBackupRetentionAndDelete(t *testing.T) {
	st, dir := newTestStore(t)
	svc := newTestService(t, dir, st)

	if err := svc.SetConfig(context.Background(), Config{RetentionCount: 2}); err != nil {
		t.Fatalf("set config: %v", err)
	}
	// 文件名秒级同拍会重名,串行等待 + 手动重命名规避:这里直接造三条记录验证裁剪 SQL。
	ids := []string{}
	for i := 0; i < 3; i++ {
		rec, err := svc.CreateBackup(context.Background(), CreateInput{Kind: "manual"})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		waitForBackup(t, svc, rec.ID)
		ids = append(ids, rec.ID)
		if i < 2 { // 最后一轮留给裁剪验证前的存在性断言
			time.Sleep(1100 * time.Millisecond)
		}
	}
	list, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("retention should keep 2, got %d", len(list))
	}
	if list[0].ID == ids[0] {
		t.Fatal("oldest backup should have been pruned")
	}
	// 删除:记录 + 磁盘文件都消失。
	if err := svc.Delete(context.Background(), list[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(svc.BackupsDir(), list[0].FileName)); !os.IsNotExist(err) {
		t.Fatal("archive file should be removed on delete")
	}
}

func TestMasterKeyPassphraseSeal(t *testing.T) {
	fake := make([]byte, 32)
	if _, err := rand.Read(fake); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPEWRIGHT_MASTER_KEY", base64.StdEncoding.EncodeToString(fake))

	sealed, err := sealMasterKeyWithPassphrase("correct horse")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(string(sealed[:3]), "pw1") == false {
		t.Fatal("sealed blob should carry pw1 prefix")
	}
	got, err := openMasterKeyWithPassphrase(sealed, "correct horse")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(got) != string(fake) {
		t.Fatal("master key roundtrip mismatch")
	}
	if _, err := openMasterKeyWithPassphrase(sealed, "wrong"); err == nil {
		t.Fatal("wrong passphrase should fail")
	}
}

func TestPendingRestoreMarker(t *testing.T) {
	dir := t.TempDir()
	p, err := ReadPendingRestore(dir)
	if err != nil || p != nil {
		t.Fatalf("no marker expected: %v %v", p, err)
	}
	if err := WritePendingRestore(dir, "restore-staging/x.pwb", ""); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	p, err = ReadPendingRestore(dir)
	if err != nil || p == nil || p.Archive != "restore-staging/x.pwb" {
		t.Fatalf("marker readback failed: %v %v", p, err)
	}
	if pass, err := p.ResolvePassphrase(); err != nil || pass != "" {
		t.Fatalf("empty passphrase should resolve empty: %q %v", pass, err)
	}
	if err := ClearPendingRestore(dir); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if p, _ := ReadPendingRestore(dir); p != nil {
		t.Fatal("marker should be gone")
	}
}

func TestFormatMySQLValue(t *testing.T) {
	cases := []struct {
		in     any
		bin    bool
		want   string
	}{
		{nil, false, "NULL"},
		{int64(42), false, "42"},
		{true, false, "1"},
		{"a'b\\c\nd", false, `'a\'b\\c\nd'`},
		{[]byte{0x00, 0xff}, true, "X'00ff'"},
		{[]byte("plain"), false, `'plain'`},
	}
	for _, c := range cases {
		if got := formatMySQLValue(c.in, c.bin); got != c.want {
			t.Errorf("formatMySQLValue(%#v,%v) = %s, want %s", c.in, c.bin, got, c.want)
		}
	}
}

func TestQuoteMySQLIdent(t *testing.T) {
	if got := quoteMySQLIdent("a`b"); got != "`a``b`" {
		t.Fatalf("got %s", got)
	}
}
