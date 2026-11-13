import json, sys

raw = open('C:/Users/xufan/AppData/Local/Temp/pve-apidoc.js', encoding='utf-8').read()
s = raw.strip()
i = raw.index('const apiSchema =') + len('const apiSchema =')
tree, _ = json.JSONDecoder().raw_decode(raw[i:].lstrip())

def find(path_parts, node):
    """按 ['nodes','{node}','tasks'] 逐层找 children"""
    for i, part in enumerate(path_parts):
        nxt = None
        for ch in node:
            if ch.get('path', ch.get('name')) == part or ch.get('text') == part:
                nxt = ch.get('children', [])
                break
        if nxt is None:
            return None
        node = nxt
    return node

def locate(path):
    """path 如 /nodes/{node}/qemu/{vmid}/status/start → 返回该节点 info"""
    parts = [p for p in path.split('/') if p]
    node = tree
    for part in parts:
        hit = None
        for ch in node:
            # apidoc.js 里路径段可能存在 'path' 或 'text' 字段
            if ch.get('path') == part or ch.get('text') == part:
                hit = ch
                break
        if hit is None:
            return None
        node = hit.get('children', [])
    return hit

def dump(path, method):
    n = locate(path)
    if n is None:
        print(f'== {method} {path}\n   !! 路径不存在')
        return
    info = n.get('info', {}).get(method)
    if info is None:
        print(f'== {method} {path}\n   !! 方法不存在(可用: {list(n.get("info",{}).keys())})')
        return
    print(f'== {method} {path}  OK')
    for p in info.get('parameters', []) or []:
        if not isinstance(p, dict):
            print(f"   param <非dict条目>: {p!r}")
            continue
        enum = p.get('enum')
        print(f"   param {p.get('name')}: type={p.get('type')} optional={p.get('optional')} enum={enum}")
    ret = info.get('returns') or {}
    props = ret.get('properties')
    if props:
        print(f"   returns fields: {sorted(props.keys())}")
    items = (ret.get('items') or {}) if isinstance(ret, dict) else {}
    # 列表返回通常在 properties 里含 items
    it2 = props.get('items') if props else None
    if it2 and isinstance(it2, dict) and it2.get('properties'):
        print(f"   returns item fields: {sorted(it2['properties'].keys())}")

for path, m in [
    ('/version', 'GET'),
    ('/cluster/status', 'GET'),
    ('/cluster/resources', 'GET'),
    ('/cluster/tasks', 'GET'),
    ('/nodes/{node}/tasks/{upid}/status', 'GET'),
]:
    dump(path, m)

# qemu / lxc 的 status 子操作
for kind in ('qemu', 'lxc'):
    n = locate(f'/nodes/{{node}}/{kind}/{{vmid}}/status')
    if n is None:
        print(f'== /nodes/{{node}}/{kind}/{{vmid}}/status  !! 不存在'); continue
    actions = [c.get('path', c.get('text')) for c in n.get('children', [])]
    print(f'== /nodes/{{node}}/{kind}/{{vmid}}/status 子操作: {sorted(actions)}')

# POST start 的 vmid 参数约束(qemu 与 lxc)
for kind in ('qemu', 'lxc'):
    dump(f'/nodes/{{node}}/{kind}/{{vmid}}/status/start', 'POST')
