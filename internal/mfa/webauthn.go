package mfa

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	webauthn "github.com/go-webauthn/webauthn/webauthn"

	"github.com/huangchengsir/pipewright/internal/systemcfg"
)

// rpDisplayName 是验证器 App 里显示的依赖方名称。
const rpDisplayName = "Pipewright"

// challengeTTL 是 WebAuthn 挑战有效期(也是挑战行的 TTL)。
const challengeTTL = 3 * time.Minute

// ChallengeTTL 导出挑战有效期(HTTP 层据此设挑战 cookie 的 MaxAge)。
const ChallengeTTL = challengeTTL

// webauthnUser 把 owner 身份适配为 go-webauthn 的 User 接口。
// user handle = owner_key("admin"/"user:<id>"),稳定且 ≤64B。
type webauthnUser struct {
	key         string
	name        string
	displayName string
	credentials []webauthn.Credential
}

func (u *webauthnUser) WebAuthnID() []byte                       { return []byte(u.key) }
func (u *webauthnUser) WebAuthnName() string                     { return u.name }
func (u *webauthnUser) WebAuthnDisplayName() string              { return u.displayName }
func (u *webauthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// rpConfig 是一次 WebAuthn 交互的依赖方配置。
type rpConfig struct {
	rpID    string
	origins []string
}

// resolveRP 推导 RP 配置:设置页显式 origins/rpID > public_url > 请求(secure+host)。
// secure/host 由 HTTP 层传入(复用 requestIsSecure 的反代识别),mfa 包不感知 HTTP。
// 非 https 且非 localhost → ErrInsecureContext(浏览器 WebAuthn 安全文本就拒绝,
// 提前报错避免用户走完一半流程)。
func (s *Service) resolveRP(ctx context.Context, secure bool, host string) (rpConfig, error) {
	var cfg systemcfg.Config
	if s.syscfg != nil {
		c, err := s.syscfg.Get(ctx)
		if err == nil {
			cfg = c
		}
	}

	// 1) 显式 origins(逗号分隔,已由设置页校验过形态)。
	if raw := strings.TrimSpace(cfg.WebAuthnOrigins); raw != "" {
		origins := splitAndTrim(raw, ",")
		rpID := strings.TrimSpace(cfg.WebAuthnRPID)
		if rpID == "" {
			rpID = hostOfOrigin(origins[0])
		}
		return rpConfig{rpID: rpID, origins: origins}, nil
	}

	// 2) public_url。
	if pu := strings.TrimSpace(cfg.PublicURL); pu != "" {
		if u, err := url.Parse(pu); err == nil && u.Host != "" {
			rpID := strings.TrimSpace(cfg.WebAuthnRPID)
			if rpID == "" {
				rpID = hostWithoutPort(u.Host)
			}
			return rpConfig{rpID: rpID, origins: []string{u.Scheme + "://" + u.Host}}, nil
		}
	}

	// 3) 请求推导。
	if host == "" {
		return rpConfig{}, fmt.Errorf("mfa: cannot derive webauthn origin (empty host)")
	}
	scheme := "http"
	if secure {
		scheme = "https"
	}
	if scheme == "http" && !isLocalhostHost(host) {
		return rpConfig{}, ErrInsecureContext
	}
	rpID := strings.TrimSpace(cfg.WebAuthnRPID)
	if rpID == "" {
		rpID = hostWithoutPort(host)
	}
	return rpConfig{rpID: rpID, origins: []string{scheme + "://" + host}}, nil
}

// webauthnFor 按 RP 配置取(缓存的)WebAuthn 实例;实例无状态,按配置缓存避免每请求重建。
func (s *Service) webauthnFor(rp rpConfig) (*webauthn.WebAuthn, error) {
	cacheKey := rp.rpID + "|" + strings.Join(rp.origins, ",")
	s.waMu.Lock()
	defer s.waMu.Unlock()
	if s.waCache == nil {
		s.waCache = make(map[string]*webauthn.WebAuthn)
	}
	if wa, ok := s.waCache[cacheKey]; ok {
		return wa, nil
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          rp.rpID,
		RPDisplayName: rpDisplayName,
		RPOrigins:     rp.origins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			// discoverable credential(passkey):登录时无需先输用户名。
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationPreferred,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: challengeTTL, TimeoutUVD: challengeTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: challengeTTL, TimeoutUVD: challengeTTL},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("mfa: build webauthn: %w", err)
	}
	s.waCache[cacheKey] = wa
	return wa, nil
}

// hostOfOrigin 取 origin 串的 host(去端口)。
func hostOfOrigin(origin string) string {
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		return hostWithoutPort(u.Host)
	}
	return hostWithoutPort(origin)
}

func hostWithoutPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// isLocalhostHost 判定 host 是否 localhost 形态(WebAuthn 安全文豁免)。
func isLocalhostHost(host string) bool {
	h := strings.ToLower(hostWithoutPort(host))
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".localhost")
}

func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
