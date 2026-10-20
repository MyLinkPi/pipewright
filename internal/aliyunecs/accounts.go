package aliyunecs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// validateAccount 校验账户字段(明文只在此函数内出现,错误信息绝不回显 ak/sk 片段)。
func validateAccount(name, ak, region string) error {
	if strings.TrimSpace(name) == "" {
		return ErrEmptyName
	}
	if strings.TrimSpace(ak) == "" {
		return ErrInvalidAccessKeyID
	}
	region = strings.TrimSpace(region)
	if region == "" || len(region) > 64 || !regionPattern.MatchString(region) {
		return ErrInvalidRegion
	}
	return nil
}

// scanAccount 把一行账户(含密文 BLOB)折为领域模型。
func scanAccount(scanner interface{ Scan(...any) error }) (*Account, error) {
	var (
		a          Account
		createdStr string
		updatedStr string
	)
	if err := scanner.Scan(&a.ID, &a.Name, &a.AccessKeyID, &a.SecretSealed, &a.Region, &createdStr, &updatedStr); err != nil {
		return nil, err
	}
	created, err := time.Parse(time.RFC3339, createdStr)
	if err != nil {
		return nil, fmt.Errorf("aliyunecs: parse created_at: %w", err)
	}
	updated, err := time.Parse(time.RFC3339, updatedStr)
	if err != nil {
		return nil, fmt.Errorf("aliyunecs: parse updated_at: %w", err)
	}
	a.CreatedAt, a.UpdatedAt = created, updated
	return &a, nil
}

const accountColumns = `id, name, access_key_id, secret_sealed, region, created_at, updated_at`

// getAccount 取单个账户(含密文;仅领域内部使用,绝不直接出响应)。
func (s *service) getAccount(ctx context.Context, id string) (*Account, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM aliyun_ecs_accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("aliyunecs: get account: %w", err)
	}
	return a, nil
}

// ListAccounts 返回全部账户(创建时间倒序)。密文 BLOB 随行返回但由调用方(httpapi DTO)
// 剥离;进程内不主动解封。
func (s *service) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+accountColumns+` FROM aliyun_ecs_accounts ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("aliyunecs: list accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("aliyunecs: scan account: %w", err)
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// CreateAccount 校验 → 封存 Secret(vault.SealSecret 密文 BLOB,同 pve_clusters 先例)→ 落库。
func (s *service) CreateAccount(ctx context.Context, in AccountCreateInput) (*Account, error) {
	name := strings.TrimSpace(in.Name)
	ak := strings.TrimSpace(in.AccessKeyID)
	region := strings.TrimSpace(in.Region)
	if err := validateAccount(name, ak, region); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Secret) == "" {
		return nil, ErrEmptySecret
	}
	if s.vault == nil {
		return nil, ErrVaultUnconfigured
	}
	sealed, err := s.vault.SealSecret([]byte(in.Secret))
	if err != nil {
		return nil, ErrVaultUnconfigured
	}

	id := newID()
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO aliyun_ecs_accounts (id, name, access_key_id, secret_sealed, region, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, name, ak, sealed, region, nowStr, nowStr,
	); err != nil {
		return nil, fmt.Errorf("aliyunecs: insert account: %w", err)
	}
	return s.getAccount(ctx, id)
}

// UpdateAccount 编辑名称/AK/Region;Secret 非空即轮换重封(credential 引用不涉及,零迁移)。
// 留空字段不修改;先读旧行再整行 UPDATE,避免部分字段被置空。
func (s *service) UpdateAccount(ctx context.Context, id string, in AccountUpdateInput) (*Account, error) {
	acc, err := s.getAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	name, ak, region := acc.Name, acc.AccessKeyID, acc.Region
	sealed := acc.SecretSealed
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	if in.AccessKeyID != nil {
		ak = strings.TrimSpace(*in.AccessKeyID)
	}
	if in.Region != nil {
		region = strings.TrimSpace(*in.Region)
	}
	if err := validateAccount(name, ak, region); err != nil {
		return nil, err
	}
	if in.Secret != nil {
		if strings.TrimSpace(*in.Secret) == "" {
			return nil, ErrEmptySecret
		}
		if s.vault == nil {
			return nil, ErrVaultUnconfigured
		}
		sealed, err = s.vault.SealSecret([]byte(*in.Secret))
		if err != nil {
			return nil, ErrVaultUnconfigured
		}
	}
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE aliyun_ecs_accounts SET name = ?, access_key_id = ?, secret_sealed = ?, region = ?, updated_at = ? WHERE id = ?`,
		name, ak, sealed, region, nowStr, id,
	); err != nil {
		return nil, fmt.Errorf("aliyunecs: update account: %w", err)
	}
	return s.getAccount(ctx, id)
}

// DeleteAccount 删除账户并连带清理其实例映射(映射无 account 外键,防删除顺序耦合;此处
// 显式两删)。找不到账户 → ErrNotFound。
func (s *service) DeleteAccount(ctx context.Context, id string) error {
	if _, err := s.getAccount(ctx, id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM aliyun_ecs_instance_links WHERE account_id = ?`, id); err != nil {
		return fmt.Errorf("aliyunecs: delete links: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM aliyun_ecs_accounts WHERE id = ?`, id); err != nil {
		return fmt.Errorf("aliyunecs: delete account: %w", err)
	}
	return nil
}

// VerifyAccount 对该账户发一次最小 DescribeInstances(PageSize=1),校验凭据/Region/权限。
func (s *service) VerifyAccount(ctx context.Context, id string) error {
	cli, _, err := s.clientFor(ctx, id)
	if err != nil {
		return err
	}
	if err := cli.Verify(ctx); err != nil {
		return fmt.Errorf("%w:%v", ErrVerifyFailed, unwrapTail(err))
	}
	return nil
}

// ListInstances 拉取该账户 Region 下的全部 ECS 实例(导入选型数据源;护栏见客户端)。
// 客户端错误统一保证 ErrListFailed 错误族(注入的假客户端可能是裸错误,一并包裹)。
func (s *service) ListInstances(ctx context.Context, accountID string) ([]Instance, error) {
	cli, _, err := s.clientFor(ctx, accountID)
	if err != nil {
		return nil, err
	}
	insts, err := cli.ListInstances(ctx)
	if err != nil && !errors.Is(err, ErrListFailed) {
		return nil, fmt.Errorf("%w:%v", ErrListFailed, err)
	}
	return insts, err
}

// unwrapTail 取 "哨兵:人话" 形态错误的人话半段(Verify 的错误体给人读,无凭据)。
func unwrapTail(err error) string {
	s := err.Error()
	if i := strings.Index(s, ":"); i >= 0 && i+1 < len(s) {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}
