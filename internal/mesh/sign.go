// sign.go 是节点间请求签名:应用层身份认证(叠加在 EasyTier 网络层加密之上)。
//
// 签名串(ed25519,base64 编码放 X-PW-Mesh-Sig):
//
//		METHOD + "\n" + path + "\n" + bodyHash + "\n" + timestamp
//
//	  - bodyHash 为请求头 X-PW-Mesh-Body 的值(小 JSON 请求 = body 的 sha256 hex,由发送方
//	    计算并随头携带;流式推送等大体量请求省略该头,签名串取 "-",完整性改由应用层逐 blob
//	    sha256 校验兜底)。接收方不读 body 即可验签,避免为验签缓冲整个上传流。
//	  - timestamp 为 Unix 秒;接收方校验 ±SignatureWindow 防重放。
package mesh

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 节点间签名头。
const (
	// HeaderNode 携带发送方 node_id。
	HeaderNode = "X-PW-Mesh-Node"
	// HeaderTS 携带 Unix 秒时间戳(防重放窗口锚点)。
	HeaderTS = "X-PW-Mesh-Ts"
	// HeaderSig 携带 base64(ed25519 签名)。
	HeaderSig = "X-PW-Mesh-Sig"
	// HeaderBody 携带 body 的 sha256 hex(小请求完整性绑定;流式请求省略)。
	HeaderBody = "X-PW-Mesh-Body"
)

// SignatureWindow 是时间戳防重放窗口(时钟偏差容忍 ±5 分钟)。
const SignatureWindow = 5 * time.Minute

// ErrBadSignature 表示签名缺失/格式错/验签失败/时间窗外的统一错误(不区分细节,防探测)。
var ErrBadSignature = errors.New("mesh: bad request signature")

// SigningPayload 拼出待签名串。method/path 取规范形(大写 method + 原样 path);
// bodyHash 为 HeaderBody 值或 "-"(流式)。
func SigningPayload(method, path, bodyHash, ts string) []byte {
	if bodyHash == "" {
		bodyHash = "-"
	}
	return []byte(strings.ToUpper(method) + "\n" + path + "\n" + bodyHash + "\n" + ts)
}

// SignHeaders 为出站请求计算并附加三枚签名头(body 为小请求体;nil/空 → 流式 "-")。
func SignHeaders(h http.Header, method, path string, body []byte, nodeID string, priv ed25519.PrivateKey) error {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	bodyHash := "-"
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		bodyHash = hex.EncodeToString(sum[:])
		h.Set(HeaderBody, bodyHash)
	}
	sig := ed25519.Sign(priv, SigningPayload(method, path, bodyHash, ts))
	h.Set(HeaderNode, nodeID)
	h.Set(HeaderTS, ts)
	h.Set(HeaderSig, base64.StdEncoding.EncodeToString(sig))
	return nil
}

// VerifyRequest 用发送方公钥(hex)校验一次入站请求的签名头。
// 校验通过返回 nil;头缺失/格式错/时间窗外/验签失败统一返回 ErrBadSignature。
func VerifyRequest(r *http.Request, publicKeyHex string) error {
	nodeID := strings.TrimSpace(r.Header.Get(HeaderNode))
	tsRaw := strings.TrimSpace(r.Header.Get(HeaderTS))
	sigRaw := strings.TrimSpace(r.Header.Get(HeaderSig))
	if nodeID == "" || tsRaw == "" || sigRaw == "" {
		return ErrBadSignature
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	now := time.Now().Unix()
	if d := now - ts; d > int64(SignatureWindow/time.Second) || d < -int64(SignatureWindow/time.Second) {
		return ErrBadSignature
	}
	pub, err := hex.DecodeString(strings.TrimSpace(publicKeyHex))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return ErrBadSignature
	}
	sig, err := base64.StdEncoding.DecodeString(sigRaw)
	if err != nil {
		return ErrBadSignature
	}
	payload := SigningPayload(r.Method, r.URL.Path, r.Header.Get(HeaderBody), tsRaw)
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		return ErrBadSignature
	}
	return nil
}
