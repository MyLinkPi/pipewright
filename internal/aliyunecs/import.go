package aliyunecs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 内联凭据的 Kind 枚举与默认凭据展示名前缀。
const (
	inlineSecretPassword = "password"
	inlineSecretKey      = "key"
	inlineCredNamePrefix = "阿里云 ECS · "
)

// pickHost 按 IP 偏好取连接地址:private(默认)取首个私网 IP,public 取首个公网 IP
// (公网来源已合并分配公网 IP 与 EIP)。取不到返回空串,由调用方记逐台错误。
func pickHost(inst Instance, preference string) string {
	if preference == IPPreferencePublic {
		if len(inst.PublicIPs) > 0 {
			return inst.PublicIPs[0]
		}
		return ""
	}
	if len(inst.PrivateIPs) > 0 {
		return inst.PrivateIPs[0]
	}
	return ""
}

// resolveImportCredential 把「每批统一」的认证入参解析为最终落库的凭据 ID:
// 选已有凭据直接透传(target 层会校验存在与类型);内联输入则先经 vault 代建一条
// ssh_password / ssh_key 凭据(整批共用一条)。返回的 ID 之外绝不外泄明文。
func (s *service) resolveImportCredential(ctx context.Context, in ImportInput, region string) (string, error) {
	hasCred := strings.TrimSpace(in.CredentialID) != ""
	hasInline := in.InlineSecret != nil
	if hasCred == hasInline { // 两者都空或都填 → 非法(须二选一)
		return "", ErrInvalidCredential
	}
	if hasCred {
		return strings.TrimSpace(in.CredentialID), nil
	}
	sec := in.InlineSecret
	if sec.Kind != inlineSecretPassword && sec.Kind != inlineSecretKey {
		return "", ErrInvalidCredential
	}
	if strings.TrimSpace(sec.Secret) == "" {
		return "", ErrInvalidCredential
	}
	if sec.Kind == inlineSecretKey && !strings.Contains(sec.Secret, "PRIVATE KEY") {
		return "", fmt.Errorf("%w:私钥内容不是 PEM(须含 -----BEGIN ... PRIVATE KEY----- 头)", ErrInvalidCredential)
	}
	if s.vault == nil {
		return "", ErrVaultUnconfigured
	}
	name := strings.TrimSpace(sec.Name)
	if name == "" {
		name = inlineCredNamePrefix + region
	}
	credType := vault.TypeSSHPassword
	if sec.Kind == inlineSecretKey {
		credType = vault.TypeSSHKey
	}
	cred, err := s.vault.Create(vault.CreateInput{
		Name:   name,
		Type:   credType,
		Secret: sec.Secret,
	})
	if err != nil {
		return "", fmt.Errorf("aliyunecs: create inline credential: %w", err)
	}
	return cred.ID, nil
}

// lookupLink 查「实例 ID ↔ servers 行」映射;无映射或查询失败返回空串(退回 host 兜底)。
func (s *service) lookupLink(ctx context.Context, accountID, instanceID string) string {
	var serverID string
	err := s.db.QueryRowContext(ctx,
		`SELECT server_id FROM aliyun_ecs_instance_links WHERE account_id = ? AND instance_id = ?`,
		accountID, instanceID).Scan(&serverID)
	if err != nil {
		return ""
	}
	return serverID
}

// upsertLink 写/刷新映射(方言尾句经 store.UpsertSuffix 生成,SQLite/MySQL 双兼容)。
func (s *service) upsertLink(ctx context.Context, accountID, instanceID, serverID string) error {
	suffix := store.UpsertSuffix(store.DialectOf(s.db),
		[]string{"account_id", "instance_id"},
		[]string{"server_id", "updated_at"})
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO aliyun_ecs_instance_links (account_id, instance_id, server_id, updated_at)
		 VALUES (?, ?, ?, ?) `+suffix,
		accountID, instanceID, serverID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("aliyunecs: upsert link: %w", err)
	}
	return nil
}

// importItemErr 把单台导入失败映射为人话(绝不含凭据明文)。
func importItemErr(err error) string {
	switch {
	case errors.Is(err, target.ErrNotFound):
		return "目标机器行不存在(可能刚被删除)"
	case errors.Is(err, target.ErrCredentialNotFound):
		return "所选 SSH 凭据不存在"
	case errors.Is(err, target.ErrCredentialTypeMismatch):
		return "所选 SSH 凭据类型不符(须为 ssh_key / ssh_password)"
	case errors.Is(err, target.ErrInvalidCredential):
		return "所选 SSH 凭据不可用(非 ssh_key / ssh_password 类型)"
	case errors.Is(err, target.ErrEmptyHost):
		return "实例 IP 为空"
	case errors.Is(err, target.ErrInvalidPort):
		return "端口非法(须为 1..65535)"
	case errors.Is(err, target.ErrPoolNotFound):
		return "默认服务器池不存在"
	case errors.Is(err, target.ErrVaultUnconfigured):
		return "保险库未配置 master key,无法校验 SSH 凭据"
	default:
		return "导入失败:" + err.Error()
	}
}

// Import 批量导入/更新所选实例(逐台独立容错,一台失败不影响整批;详见包注释)。
func (s *service) Import(ctx context.Context, in ImportInput) (*ImportResult, error) {
	// 入参归一与校验(批级错误 → 整批 4xx,不落任何库)。
	seen := make(map[string]bool, len(in.InstanceIDs))
	ids := make([]string, 0, len(in.InstanceIDs))
	for _, id := range in.InstanceIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, ErrNoInstances
	}
	if len(ids) > MaxImportBatch {
		return nil, ErrTooManyInstances
	}
	pref := in.IPPreference
	if pref == "" {
		pref = IPPreferencePrivate
	}
	if pref != IPPreferencePrivate && pref != IPPreferencePublic {
		return nil, ErrInvalidIPPreference
	}
	user := strings.TrimSpace(in.SSHUser)
	if user == "" {
		return nil, ErrInvalidSSHUser
	}
	port := in.SSHPort
	if port <= 0 {
		port = target.DefaultPort
	}

	// 账户 + 实例清单(以后端现拉为准,防前端选型数据过期)。
	cli, acc, err := s.clientFor(ctx, in.AccountID)
	if err != nil {
		return nil, err
	}
	instances, err := cli.ListInstances(ctx)
	if err != nil {
		if errors.Is(err, ErrListFailed) {
			return nil, err
		}
		return nil, fmt.Errorf("%w:%v", ErrListFailed, err)
	}
	byID := make(map[string]Instance, len(instances))
	for _, inst := range instances {
		byID[inst.InstanceID] = inst
	}

	// host 兜底匹配表(跳过 PVE 物理机与控制节点伪机:误更新会改坏系统管理行)。
	servers, err := s.servers.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("aliyunecs: list servers: %w", err)
	}
	byHost := make(map[string]string, len(servers))
	for _, srv := range servers {
		if srv.Kind != target.KindStandard {
			continue
		}
		if _, dup := byHost[srv.Host]; !dup {
			byHost[srv.Host] = srv.ID
		}
	}

	// 认证解析(可能代建内联凭据;失败 → 整批拒绝,不产生半批落库)。
	credID, err := s.resolveImportCredential(ctx, in, acc.Region)
	if err != nil {
		return nil, err
	}

	res := &ImportResult{Total: len(ids), Items: make([]ImportItemResult, 0, len(ids))}
	for _, iid := range ids {
		item := ImportItemResult{InstanceID: iid}
		inst, ok := byID[iid]
		if !ok {
			item.Err = "实例不存在(可能已释放或不属于该账户)"
			res.Failed++
			res.Items = append(res.Items, item)
			continue
		}
		item.Name = inst.Name
		host := pickHost(inst, pref)
		if host == "" {
			if pref == IPPreferencePublic {
				item.Err = "该实例没有公网 IP(未分配且未绑 EIP)"
			} else {
				item.Err = "该实例没有内网 IP"
			}
			res.Failed++
			res.Items = append(res.Items, item)
			continue
		}
		name := inst.Name
		if strings.TrimSpace(name) == "" {
			name = iid
		}

		// 更新匹配:实例 ID 映射(首选,IP 变化仍命中)→ 连接 IP 兜底(兼容手工登记过的行)。
		serverID := s.lookupLink(ctx, acc.ID, iid)
		if serverID != "" {
			if _, err := s.servers.Get(ctx, serverID); err != nil {
				serverID = "" // 映射悬挂(机器行已删):降级走兜底/新建,并待会重写映射。
			}
		}
		if serverID == "" {
			serverID = byHost[host]
		}

		if serverID != "" {
			item.Action = "updated"
			srv, err := s.servers.Update(ctx, serverID, target.UpdateInput{
				Name:         &name,
				Host:         &host,
				Port:         &port,
				User:         &user,
				CredentialID: &credID,
			})
			if err != nil {
				item.Err = importItemErr(err)
			} else {
				item.OK = true
				item.ServerID = srv.ID
			}
		} else {
			item.Action = "created"
			srv, err := s.servers.Create(ctx, target.CreateInput{
				Name:         name,
				Host:         host,
				Port:         port,
				User:         user,
				CredentialID: credID,
			})
			if err != nil {
				item.Err = importItemErr(err)
			} else {
				item.OK = true
				item.ServerID = srv.ID
			}
		}
		if item.OK {
			res.OK++
			// 成功即写/刷新映射(更新路径也重写:兜底命中的行从此挂到该实例)。
			if err := s.upsertLink(ctx, acc.ID, iid, item.ServerID); err != nil {
				// 映射失败不回滚落库(机器行已生效;仅下次导入退回 host 兜底)。错误不外抛,
				// 但也不能静默吞:并入该台结果,提示用户。
				item.Err = "已落库,但实例映射写入失败(下次导入可能无法按实例 ID 命中)"
			}
		} else {
			res.Failed++
		}
		res.Items = append(res.Items, item)
	}
	return res, nil
}
