import json
raw = open(r'C:/Users/xufan/AppData/Local/Temp/pve-apidoc.js', encoding='utf-8').read()
i = raw.index('const apiSchema =') + len('const apiSchema =')
tree, _ = json.JSONDecoder().raw_decode(raw[i:].lstrip())

def locate(path):
    node = tree; hit = None
    for part in [p for p in path.split('/') if p]:
        for ch in node:
            if ch.get('path') == part or ch.get('text') == part:
                hit = ch; break
        else:
            return None
        node = hit.get('children', [])
    return hit

def params_of(info):
    ps = info.get('parameters')
    if isinstance(ps, list):
        return [p for p in ps if isinstance(p, dict)]
    if isinstance(ps, dict):
        return list((ps.get('properties') or {}).values())
    return []

def item_fields(ret):
    """returns 列表项字段"""
    if not isinstance(ret, dict): return None
    props = ret.get('properties') or {}
    it = props.get('items')
    if isinstance(it, dict):
        ip = it.get('properties')
        if ip: return sorted(ip.keys())
    return None

for path, m in [
    ('/cluster/tasks', 'GET'),
    ('/cluster/status', 'GET'),
    ('/cluster/resources', 'GET'),
]:
    n = locate(path)
    info = n['info'][m]
    print(f'== {m} {path}')
    for p in params_of(info):
        print(f"   param {p.get('name')}: enum={p.get('enum')} type={p.get('type')}")
    f = item_fields(info.get('returns'))
    print(f'   item fields: {f}')

for kind in ('qemu', 'lxc'):
    n = locate(f'/nodes/{{node}}/{kind}/{{vmid}}/status/start')
    info = n['info']['POST']
    print(f'== POST /nodes/{{node}}/{kind}/{{vmid}}/status/start 路径参数:')
    for p in params_of(info):
        if p.get('name') in ('node', 'vmid'):
            print(f"   {p.get('name')}: type={p.get('type')} min={p.get('minimum')} max={p.get('maximum')} pattern={p.get('pattern')!r}")

# lxc reset 确认不存在
print('lxc reset exists:', locate('/nodes/{node}/lxc/{vmid}/status/reset') is not None)
print('qemu reset exists:', locate('/nodes/{node}/qemu/{vmid}/status/reset') is not None)
