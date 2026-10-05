# -*- coding: utf-8 -*-
"""修复 ja/ko pipelineJob:撤回插错的 jobRunnerHint 行,改插到 fieldImageHint 语句之后。"""
import io, re

FIX = {
 'ja': ('  jobRunnerHint: ', 'ビルドイメージ相当)",\n'),
 'ko': ('  jobRunnerHint: ', '빌드 이미지에 상당)",\n'),
}
HINT = {
 'ja': '空 = ステージ/プロジェクト既定に従う(ステージはノード設定、プロジェクトは「ビルド」タブ)。同じセレクターのノードはマシンとワークスペースを共有し、異なるセレクターのノードは別々のマシンに配備されます。',
 'ko': '비어 있으면 스테이지/프로젝트 기본값을 따릅니다(스테이지는 노드 설정, 프로젝트는 빌드 탭). 같은 셀렉터의 노드는 머신과 작업 공간을 공유하고, 다른 셀렉터의 노드는 각각 다른 머신에 배치됩니다.',
}

for loc in ('ja', 'ko'):
    p = f'web/src/i18n/locales/{loc}/pipelineJob.ts'
    s = io.open(p, encoding='utf-8').read()
    # 1) 撤回插错的整行
    s2 = re.sub(r"^  jobRunnerHint: '.*\n", '', s, flags=re.M)
    assert s2 != s, loc
    s = s2
    # 2) 找到 fieldImageHint 语句的收尾行(含 `ビルドイメージ相当)",` 等),在其后插入
    anchor = FIX[loc][1]
    idx = s.find(anchor)
    assert idx != -1, (loc, 'anchor')
    end = idx + len(anchor)
    s = s[:end] + f"  jobRunnerHint: {HINT[loc]!r},\n" + s[end:]
    io.open(p, 'w', encoding='utf-8', newline='').write(s)
    print('fixed', loc)
