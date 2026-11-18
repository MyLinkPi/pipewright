// provision.go — 建机配套只读面(模板/存储/网桥/nextid/存储内容/LXC 模板目录)
// 与建机管线共用的内部助手(snippet 存储定位等)。
//
// 只读端点不审计(参照 Overview/ListVMs);DownloadAplTemplate 是写操作(往存储落
// 模板文件),挂 readonly 闸门。入参 node/storage/content/template 白名单校验。
package pveadmin

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// contentTypes 是存储内容类型白名单(PVE 存储内容枚举)。
var contentTypes = map[string]bool{
	"images": true, "iso": true, "vztmpl": true, "backup": true,
	"snippets": true, "import": true, "rootdir": true,
}

// aplTemplateRe 是 aplinfo 模板标识白名单(如 ubuntu-24.04-standard)。
var aplTemplateRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// Template 是一个可克隆 QEMU 模板。
type Template struct {
	VMID int64  `json:"vmid"`
	Name string `json:"name"`
	Node string `json:"node"`
	Type string `json:"type"` // qemu(LXC 克隆尚未支持,不列入)
}

// Storage 是节点存储状态(建机表单目标存储/镜像存储下拉)。
type Storage struct {
	Storage string   `json:"storage"`
	Type    string   `json:"type"` // dir/nfs/lvmthin/zfs/...
	Active  bool     `json:"active"`
	Enabled bool     `json:"enabled"`
	Shared  bool     `json:"shared"`
	Content []string `json:"content"`
	Used    int64    `json:"used"`
	Total   int64    `json:"total"`
	Avail   int64    `json:"avail"`
}

// Bridge 是节点网桥(net0 bridge 下拉)。
type Bridge struct {
	Iface    string `json:"iface"`
	Type     string `json:"type"` // bridge | OVSBridge
	Active   bool   `json:"active"`
	Address  string `json:"address"`
	Comments string `json:"comments"`
}

// ContentItem 是存储内容条目(vztmpl/import/images/backup 卷列表)。
type ContentItem struct {
	Volid   string `json:"volid"`
	Content string `json:"content"`
	Format  string `json:"format"`
	Size    int64  `json:"size"`
	VMID    int64  `json:"vmid"`
}

// AplTemplate 是在线 LXC 模板目录条目(GET aplinfo)。
type AplTemplate struct {
	Template    string `json:"template"`
	Package     string `json:"package"`
	Version     string `json:"version"`
	Section     string `json:"section"`
	Description string `json:"description"`
}

// ListTemplates 列集群可克隆模板(/cluster/resources 过滤 template=1 的 qemu)。
func (s *service) ListTemplates(ctx context.Context, id string) ([]Template, error) {
	_, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	var res []apiResource
	if err := c.Get(ctx, "/api2/json/cluster/resources", &res); err != nil {
		return nil, err
	}
	out := make([]Template, 0)
	for _, r := range res {
		if r.Type != "qemu" || !bool(r.Template) {
			continue
		}
		out = append(out, Template{VMID: int64(r.VMID), Name: r.Name, Node: r.Node, Type: r.Type})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].VMID < out[j].VMID
	})
	return out, nil
}

// ListStorages 列节点存储状态(含内容类型/用量;active/enabled 由调用方过滤)。
func (s *service) ListStorages(ctx context.Context, id, node string) ([]Storage, error) {
	if !nameRe.MatchString(node) {
		return nil, fmt.Errorf("%w: 节点名不合规", ErrInvalidInput)
	}
	_, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	entries, err := storagesFor(ctx, c, node)
	if err != nil {
		return nil, err
	}
	out := make([]Storage, 0, len(entries))
	for _, e := range entries {
		out = append(out, storageView(e))
	}
	return out, nil
}

// ListBridges 列节点网桥(type=bridge/OVSBridge)。
func (s *service) ListBridges(ctx context.Context, id, node string) ([]Bridge, error) {
	if !nameRe.MatchString(node) {
		return nil, fmt.Errorf("%w: 节点名不合规", ErrInvalidInput)
	}
	_, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	var entries []apiNetworkEntry
	if err := c.Get(ctx, "/api2/json/nodes/"+node+"/network", &entries); err != nil {
		return nil, err
	}
	out := make([]Bridge, 0)
	for _, e := range entries {
		if e.Type != "bridge" && e.Type != "OVSBridge" {
			continue
		}
		out = append(out, Bridge{Iface: e.Iface, Type: e.Type, Active: bool(e.Active), Address: e.Address, Comments: e.Comments})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Iface < out[j].Iface })
	return out, nil
}

// NextVMID 取集群下一个空闲 VMID(GET /cluster/nextid;字符串数值怪癖 flexInt 收)。
func (s *service) NextVMID(ctx context.Context, id string) (int64, error) {
	_, c, err := s.clientFor(ctx, id)
	if err != nil {
		return 0, err
	}
	var next flexInt
	if err := c.Get(ctx, "/api2/json/cluster/nextid", &next); err != nil {
		return 0, err
	}
	if next < 100 {
		return 0, fmt.Errorf("%w: /cluster/nextid 返回 %d 越界", ErrConnectFailed, int64(next))
	}
	return int64(next), nil
}

// ListStorageContent 列存储内容(按内容类型过滤:vztmpl/import/images/backup...)。
func (s *service) ListStorageContent(ctx context.Context, id, node, storage, content string) ([]ContentItem, error) {
	if !nameRe.MatchString(node) || !nameRe.MatchString(storage) {
		return nil, fmt.Errorf("%w: 节点名/存储名不合规", ErrInvalidInput)
	}
	if !contentTypes[content] {
		return nil, fmt.Errorf("%w: 未知内容类型 %q", ErrInvalidInput, content)
	}
	_, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	entries, err := storageContentFor(ctx, c, node, storage, content)
	if err != nil {
		return nil, err
	}
	out := make([]ContentItem, 0, len(entries))
	for _, e := range entries {
		out = append(out, ContentItem{Volid: e.Volid, Content: e.Content, Format: e.Format, Size: int64(e.Size), VMID: int64(e.VMID)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Volid < out[j].Volid })
	return out, nil
}

// ListAplInfo 列节点可下载的 LXC 模板目录(GET aplinfo)。
func (s *service) ListAplInfo(ctx context.Context, id, node string) ([]AplTemplate, error) {
	if !nameRe.MatchString(node) {
		return nil, fmt.Errorf("%w: 节点名不合规", ErrInvalidInput)
	}
	_, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	var entries []apiAplEntry
	if err := c.Get(ctx, "/api2/json/nodes/"+node+"/aplinfo", &entries); err != nil {
		return nil, err
	}
	out := make([]AplTemplate, 0, len(entries))
	for _, e := range entries {
		out = append(out, AplTemplate{Template: e.Template, Package: e.Package, Version: e.Version, Section: e.Section, Description: e.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Template < out[j].Template })
	return out, nil
}

// DownloadAplTemplate 下载 LXC 模板到存储(POST aplinfo → UPID)。写操作:readonly 闸门。
func (s *service) DownloadAplTemplate(ctx context.Context, id, node, storage, template string) (*TaskRef, error) {
	if !nameRe.MatchString(node) || !nameRe.MatchString(storage) {
		return nil, fmt.Errorf("%w: 节点名/存储名不合规", ErrInvalidInput)
	}
	if !aplTemplateRe.MatchString(template) {
		return nil, fmt.Errorf("%w: 模板标识不合规", ErrInvalidInput)
	}
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	if cl.Readonly {
		return nil, fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再下载模板", ErrReadonly, cl.Name)
	}
	var upid string
	path := "/api2/json/nodes/" + node + "/aplinfo"
	if err := c.Post(ctx, path, url.Values{"storage": {storage}, "template": {template}}, &upid); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(upid, "UPID:") {
		return nil, fmt.Errorf("%w: %s 响应缺 UPID", ErrConnectFailed, path)
	}
	return &TaskRef{Node: node, UPID: upid}, nil
}

// ---- 建机管线内部助手 -----------------------------------------------------------------

// storagesFor 取节点存储状态原始条目。
func storagesFor(ctx context.Context, c pveClient, node string) ([]apiStorageEntry, error) {
	var entries []apiStorageEntry
	if err := c.Get(ctx, "/api2/json/nodes/"+node+"/storage", &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// storageView 把原始条目转为出响应视图(content 逗号串拆列表)。
func storageView(e apiStorageEntry) Storage {
	var content []string
	for _, part := range strings.Split(e.Content, ",") {
		if p := strings.TrimSpace(part); p != "" {
			content = append(content, p)
		}
	}
	return Storage{
		Storage: e.Storage, Type: e.Type, Active: bool(e.Active), Enabled: bool(e.Enabled),
		Shared: bool(e.Shared), Content: content, Used: int64(e.Used), Total: int64(e.Total), Avail: int64(e.Avail),
	}
}

// contentHas 判断存储内容类型串是否含指定类型。
func contentHas(content, want string) bool {
	for _, part := range strings.Split(content, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

// storageContentFor 取存储内容原始条目(按类型过滤)。
func storageContentFor(ctx context.Context, c pveClient, node, storage, content string) ([]apiContentEntry, error) {
	q := url.Values{"content": {content}}.Encode()
	var entries []apiContentEntry
	path := "/api2/json/nodes/" + node + "/storage/" + storage + "/content?" + q
	if err := c.Get(ctx, path, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// storageConfig 取数据中心级存储配置(GET /storage/{storage};dir/nfs 含 path)。
// 返回 map 防御性收(各插件字段不一)。
func storageConfig(ctx context.Context, c pveClient, storage string) (map[string]any, error) {
	var cfg map[string]any
	if err := c.Get(ctx, "/api2/json/storage/"+storage, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// snippetsStorage 定位节点上启用 snippets 内容的存储(优先 local),返回存储 ID 与
// 文件系统路径(cloud-init snippet 经 SSH 落盘用)。无可用存储/取不到路径 → 可操作错误。
func snippetsStorage(ctx context.Context, c pveClient, node string) (string, string, error) {
	entries, err := storagesFor(ctx, c, node)
	if err != nil {
		return "", "", err
	}
	var cands []apiStorageEntry
	for _, e := range entries {
		if bool(e.Active) && bool(e.Enabled) && contentHas(e.Content, "snippets") {
			cands = append(cands, e)
		}
	}
	if len(cands) == 0 {
		return "", "", fmt.Errorf("%w: 节点 %s 无启用 snippets 内容类型的存储,无法注入 cloud-init 自定义配置", ErrInvalidInput, node)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if (cands[i].Storage == "local") != (cands[j].Storage == "local") {
			return cands[i].Storage == "local"
		}
		return cands[i].Storage < cands[j].Storage
	})
	for _, e := range cands {
		cfg, cerr := storageConfig(ctx, c, e.Storage)
		if cerr != nil {
			continue
		}
		if path, _ := cfg["path"].(string); path != "" {
			return e.Storage, path, nil
		}
	}
	return "", "", fmt.Errorf("%w: 节点 %s 的 snippets 存储均为非文件型(取不到路径),无法经 SSH 写入 cloud-init 配置", ErrInvalidInput, node)
}
