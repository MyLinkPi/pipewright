; pwclient.nsi —— Pipewright 集群客户端 Windows 安装包(NSIS 3)
;
; 由 scripts/build-installer.sh 调 makensis 编译;所有可变项经 -D 传入(见脚本)。
; 设计要点(对应「用安装包替代界面里点击安装服务」的诉求):
;   1. 安装段先「优雅停服务 + 兜底杀残留进程」,解开 pwclient.exe 与 bin/easytier-*.exe
;      的文件占用,再覆盖到 %ProgramFiles%\Pipewright Client —— 从根上避开旧「点安装服务
;      后必须手动进 Program Files 运行才连上」的时序坑(文件被占用 / 服务未真正拉起)。
;   2. 覆盖后以安装目录里的新 exe 执行 `pwclient service install`:exe 已在 Program Files,
;      installService 不再二次复制,直接 CreateService + Start(开机自启、LocalSystem、免登录即连)。
;   3. 完成页勾选「启动 Pipewright 客户端」→ 装完即拉起托盘 GUI,引导导入集群配置(安装完启动)。
;   4. 卸载段先 `pwclient service uninstall`(停 + 删服务)再清理文件 / 快捷方式 / 注册表。
;
; 编译期变量(makensis -D):
;   PW_SRC      暂存目录(解包后的 pwclient zip:pwclient.exe / bin/ / licenses/ / LICENSE)  必填
;   PW_VERSION  版本号文本(展示 + 注册表)                                                必填
;   PW_OUTFILE  输出安装包 exe 文件名                                                       必填
;   PW_ICON     图标 .ico 路径(默认取仓库 cmd/pwclient/winres/icon.ico)
;   PW_LICENSE  许可协议文件路径(默认取暂存目录 LICENSE)
;   PW_VI_VER   4 段数字版本(x.x.x.x,写 PE 版本资源;非法则跳过)

Unicode True
!include "MUI2.nsh"

; ---- 变量与默认值 ----
!ifndef PW_SRC
  !error "PW_SRC 未定义:需传入解包后的客户端目录(makensis -DPW_SRC=...)"
!endif
!ifndef PW_VERSION
  !define PW_VERSION "0.0.0"
!endif
!ifndef PW_OUTFILE
  !define PW_OUTFILE "pwclient_setup.exe"
!endif
!ifndef PW_ICON
  ; 编译期主机路径(Linux CI 用正斜杠;Windows 本地 makensis 亦归一化)。
  !define PW_ICON "cmd/pwclient/winres/icon.ico"
!endif
!ifndef PW_LICENSE
  !define PW_LICENSE "${PW_SRC}/LICENSE"
!endif

; ---- 基本元数据 ----
Name "Pipewright Client"
OutFile "${PW_OUTFILE}"
InstallDir "$PROGRAMFILES64\Pipewright Client"
InstallDirRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\PipewrightClient" "InstallLocation"
RequestExecutionLevel admin
SetCompressor /SOLID lzma

!ifdef PW_VI_VER
  VIProductVersion "${PW_VI_VER}"
!endif
VIAddVersionKey /LANG=2052 "ProductName" "Pipewright Client"
VIAddVersionKey /LANG=2052 "ProductVersion" "${PW_VERSION}"
VIAddVersionKey /LANG=2052 "FileDescription" "Pipewright 集群客户端安装程序"
VIAddVersionKey /LANG=2052 "LegalCopyright" ""

; ---- 界面 ----
!define MUI_ABORTWARNING
!define MUI_ICON "${PW_ICON}"
!define MUI_UNICON "${PW_ICON}"

!define MUI_WELCOMEPAGE_TITLE "欢迎安装 Pipewright 集群客户端"
!define MUI_WELCOMEPAGE_TEXT "本向导将把 Pipewright 集群客户端安装到本机,并注册为开机自启的后台服务(LocalSystem,开机即连,无需登录)。$\n$\n点击“下一步”继续。"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "${PW_LICENSE}"
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES

; 完成页:安装完即启动托盘 GUI(勾选默认开)。
!define MUI_FINISHPAGE_RUN "$INSTDIR\pwclient.exe"
!define MUI_FINISHPAGE_RUN_TEXT "启动 Pipewright 客户端(托盘)"
!define MUI_FINISHPAGE_RUN_CHECKED
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "SimpChinese"
!insertmacro MUI_LANGUAGE "English"

; ---- 安装 ----
Section "客户端程序与服务" SEC_INSTALL
  SectionIn RO

  ; 1) 停旧服务 + 兜底清残留进程,解开 exe / bin 文件占用(全部忽略「未安装 / 未在运行」错误)。
  DetailPrint "停止旧服务与运行实例…"
  ExecWait 'net stop "pipewright-client"' $0
  ExecWait 'taskkill /F /IM pwclient.exe' $0
  ExecWait 'taskkill /F /IM easytier-core.exe' $0
  ExecWait 'taskkill /F /IM easytier-cli.exe' $0

  ; 2) 覆盖文件到 Program Files(64 位视图写注册表)。File 源为编译期主机路径(正斜杠)。
  SetRegView 64
  SetOutPath "$INSTDIR"
  File "${PW_SRC}/pwclient.exe"
  SetOutPath "$INSTDIR\bin"
  File /r "${PW_SRC}/bin/*"
  SetOutPath "$INSTDIR\licenses"
  File /r "${PW_SRC}/licenses/*"

  ; 3) 注册并立即启动后台服务(exe 已在 Program Files,installService 不再二次复制)。
  ;    安装器已提权,子进程继承管理员令牌 → 服务管理器连接成功,不会再弹第二次 UAC。
  DetailPrint "注册并启动后台服务…"
  ExecWait '"$INSTDIR\pwclient.exe" service install' $0

  ; 4) 快捷方式。
  CreateDirectory "$SMPROGRAMS\Pipewright Client"
  CreateShortcut "$SMPROGRAMS\Pipewright Client\Pipewright Client.lnk" "$INSTDIR\pwclient.exe" "" "$INSTDIR\pwclient.exe" 0
  CreateShortcut "$DESKTOP\Pipewright Client.lnk" "$INSTDIR\pwclient.exe" "" "$INSTDIR\pwclient.exe" 0

  ; 5) 卸载信息与安装位置记录。
  WriteUninstaller "$INSTDIR\uninstall.exe"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\PipewrightClient" "DisplayName" "Pipewright Client"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\PipewrightClient" "Publisher" "pipewright"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\PipewrightClient" "DisplayVersion" "${PW_VERSION}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\PipewrightClient" "DisplayIcon" "$INSTDIR\pwclient.exe"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\PipewrightClient" "InstallLocation" "$INSTDIR"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\PipewrightClient" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\PipewrightClient" "NoModify" 1
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\PipewrightClient" "NoRepair" 1
SectionEnd

; ---- 卸载 ----
Section "Uninstall"
  SetRegView 64

  ; 先停 + 删服务(sc delete 依赖服务已停,uninstallService 内部已 stopService)。
  ExecWait '"$INSTDIR\pwclient.exe" service uninstall' $0
  ExecWait 'net stop "pipewright-client"' $0
  ExecWait 'taskkill /F /IM pwclient.exe' $0

  Delete "$DESKTOP\Pipewright Client.lnk"
  Delete "$SMPROGRAMS\Pipewright Client\Pipewright Client.lnk"
  RMDir "$SMPROGRAMS\Pipewright Client"

  RMDir /r "$INSTDIR\bin"
  RMDir /r "$INSTDIR\licenses"
  Delete "$INSTDIR\pwclient.exe"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"

  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\PipewrightClient"
SectionEnd
