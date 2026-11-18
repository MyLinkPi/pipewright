package i18n

// PVE 集群纳管与管理(服务器池 0084)的 API 消息。Token 密文绝不入消息;
// 动态 detail(PVE 错误文本等)经前缀词条透传。
func init() {
	register(map[string]map[string]string{
		"PVE 集群不存在": {
			"zh-HK": "PVE 叢集不存在", "en": "PVE cluster not found", "ja": "PVE クラスターが存在しません",
			"ko": "PVE 클러스터가 없습니다", "es": "clúster PVE no encontrado", "fr": "cluster PVE introuvable", "de": "PVE-Cluster nicht gefunden",
		},
		"同名 PVE 集群已存在": {
			"zh-HK": "同名 PVE 叢集已存在", "en": "a PVE cluster with the same name already exists", "ja": "同名の PVE クラスターが既に存在します",
			"ko": "같은 이름의 PVE 클러스터가 이미 있습니다", "es": "ya existe un clúster PVE con el mismo nombre", "fr": "un cluster PVE du même nom existe déjà", "de": "ein PVE-Cluster mit demselben Namen existiert bereits",
		},
		"入参非法:字段格式或取值不符合要求": {
			"zh-HK": "入參非法:欄位格式或取值不符合要求", "en": "invalid input: field format or value does not meet requirements", "ja": "入力が不正です:フィールドの形式または値が要件を満たしていません",
			"ko": "입력이 잘못되었습니다: 필드 형식 또는 값이 요구 사항에 맞지 않습니다", "es": "entrada no válida: el formato o el valor de un campo no cumple los requisitos", "fr": "entrée non valide : le format ou la valeur d’un champ ne respecte pas les exigences", "de": "ungültige Eingabe: Feldformat oder -wert erfüllt die Anforderungen nicht",
		},
		"备份上传超过 2 TiB 上限": {
			"zh-HK": "備份上傳超過 2 TiB 上限", "en": "backup upload exceeds the 2 TiB limit", "ja": "バックアップのアップロードが 2 TiB の上限を超えています",
			"ko": "백업 업로드가 2 TiB 제한을 초과했습니다", "es": "la carga de la copia de seguridad supera el límite de 2 TiB", "fr": "le téléversement de la sauvegarde dépasse la limite de 2 Tio", "de": "Backup-Upload überschreitet das Limit von 2 TiB",
		},
		"保险库未配置 master key,无法存取 API Token": {
			"zh-HK": "保險庫未設定 master key,無法存取 API Token", "en": "vault has no master key configured; cannot store API tokens", "ja": "vault に master key が設定されていないため、API Token を保存できません",
			"ko": "vault에 master key가 구성되지 않아 API Token을 저장할 수 없습니다", "es": "la bóveda no tiene master key configurada; no se pueden guardar tokens API", "fr": "le coffre-fort n'a pas de master key configurée ; impossible de stocker les tokens API", "de": "Vault hat keinen master key konfiguriert; API-Tokens können nicht gespeichert werden",
		},
		"集群尚未配置可用的 API Token,请先在设置页完成配置": {
			"zh-HK": "叢集尚未配置可用的 API Token,請先在設定頁完成配置", "en": "cluster has no usable API token yet; configure it on the settings page first", "ja": "クラスターに利用可能な API Token がまだありません。先に設定ページで構成してください",
			"ko": "클러스터에 사용 가능한 API Token이 아직 없습니다. 먼저 설정 페이지에서 구성하세요", "es": "el clúster aún no tiene un token API utilizable; configúrelo primero en la página de ajustes", "fr": "le cluster n'a pas encore de token API utilisable ; configurez-le d'abord dans les paramètres", "de": "Cluster hat noch keinen verwendbaren API-Token; konfigurieren Sie ihn zuerst auf der Einstellungsseite",
		},
		"集群处于只读模式,请先在设置页解锁后再执行写操作": {
			"zh-HK": "叢集處於唯讀模式,請先在設定頁解鎖後再執行寫操作", "en": "cluster is read-only; unlock it on the settings page before write operations", "ja": "クラスターは読み取り専用です。書き込み操作の前に設定ページでロック解除してください",
			"ko": "클러스터가 읽기 전용입니다. 쓰기 작업 전에 설정 페이지에서 잠금을 해제하세요", "es": "el clúster está en modo de solo lectura; desbloquéelo en ajustes antes de operaciones de escritura", "fr": "le cluster est en lecture seule ; déverrouillez-le dans les paramètres avant les opérations d'écriture", "de": "Cluster ist im Nur-Lese-Modus; entsperren Sie ihn in den Einstellungen vor Schreibaktionen",
		},
		"该集群已有建机任务在运行,请等待完成": {
			"zh-HK": "該叢集已有建機任務在執行,請等待完成", "en": "a VM creation job is already running on this cluster; wait for it to finish", "ja": "このクラスターでは VM 作成ジョブが実行中です。完了をお待ちください",
			"ko": "이 클러스터에서 VM 생성 작업이 이미 실행 중입니다. 완료될 때까지 기다리세요", "es": "ya hay un trabajo de creación de VM en este clúster; espere a que termine", "fr": "une tâche de création de VM est déjà en cours sur ce cluster ; attendez qu'elle se termine", "de": "auf diesem Cluster läuft bereits ein VM-Erstellungsauftrag; warten Sie auf den Abschluss",
		},
		"缺少 node 查询参数": {
			"zh-HK": "缺少 node 查詢參數", "en": "missing node query parameter", "ja": "node クエリパラメータがありません",
			"ko": "node 쿼리 매개변수가 없습니다", "es": "falta el parámetro node", "fr": "paramètre node manquant", "de": "Parameter node fehlt",
		},
		"缺少 node/storage 查询参数": {
			"zh-HK": "缺少 node/storage 查詢參數", "en": "missing node/storage query parameters", "ja": "node/storage クエリパラメータがありません",
			"ko": "node/storage 쿼리 매개변수가 없습니다", "es": "faltan parámetros node/storage", "fr": "paramètres node/storage manquants", "de": "Parameter node/storage fehlen",
		},
		"缺少 node/storage/content 查询参数": {
			"zh-HK": "缺少 node/storage/content 查詢參數", "en": "missing node/storage/content query parameters", "ja": "node/storage/content クエリパラメータがありません",
			"ko": "node/storage/content 쿼리 매개변수가 없습니다", "es": "faltan parámetros node/storage/content", "fr": "paramètres node/storage/content manquants", "de": "Parameter node/storage/content fehlen",
		},
		"请求须为 multipart/form-data": {
			"zh-HK": "請求須為 multipart/form-data", "en": "request must be multipart/form-data", "ja": "リクエストは multipart/form-data である必要があります",
			"ko": "요청은 multipart/form-data여야 합니다", "es": "la petición debe ser multipart/form-data", "fr": "la requête doit être multipart/form-data", "de": "Anfrage muss multipart/form-data sein",
		},
		"缺少文件字段 filename": {
			"zh-HK": "缺少檔案欄位 filename", "en": "missing file field filename", "ja": "ファイルフィールド filename がありません",
			"ko": "파일 필드 filename이 없습니다", "es": "falta el campo de archivo filename", "fr": "champ de fichier filename manquant", "de": "Dateifeld filename fehlt",
		},
		"multipart 解析失败": {
			"zh-HK": "multipart 解析失敗", "en": "failed to parse multipart body", "ja": "multipart の解析に失敗しました",
			"ko": "multipart 본문 분석에 실패했습니다", "es": "fallo al analizar multipart", "fr": "échec d'analyse multipart", "de": "Multipart-Analyse fehlgeschlagen",
		},
		"PVE 集群物理机不允许作为部署目标": {
			"zh-HK": "PVE 叢集實體機不允許作為部署目標", "en": "PVE cluster hosts cannot be deployment targets", "ja": "PVE クラスターの物理マシンをデプロイ対象にすることはできません",
			"ko": "PVE 클러스터 물리 머신은 배포 대상이 될 수 없습니다", "es": "las máquinas físicas del clúster PVE no pueden ser destinos de despliegue", "fr": "les machines physiques du cluster PVE ne peuvent pas être des cibles de déploiement", "de": "physische PVE-Cluster-Maschinen können keine Bereitstellungsziele sein",
		},
		"PVE 集群物理机不允许作为应用部署目标": {
			"zh-HK": "PVE 叢集實體機不允許作為應用部署目標", "en": "PVE cluster hosts cannot be app deployment targets", "ja": "PVE クラスターの物理マシンをアプリデプロイ対象にすることはできません",
			"ko": "PVE 클러스터 물리 머신은 앱 배포 대상이 될 수 없습니다", "es": "las máquinas físicas del clúster PVE no pueden ser destinos de despliegue de apps", "fr": "les machines physiques du cluster PVE ne peuvent pas être des cibles de déploiement d'applications", "de": "physische PVE-Cluster-Maschinen können keine App-Bereitstellungsziele sein",
		},
		"vmid 非法": {
			"zh-HK": "vmid 非法", "en": "invalid vmid", "ja": "vmid が不正です",
			"ko": "vmid가 잘못되었습니다", "es": "vmid no válido", "fr": "vmid non valide", "de": "ungültige vmid",
		},
		"缺少 node/upid 查询参数": {
			"zh-HK": "缺少 node/upid 查詢參數", "en": "missing node/upid query parameters", "ja": "node/upid クエリパラメータがありません",
			"ko": "node/upid 쿼리 매개변수가 없습니다", "es": "faltan parámetros node/upid", "fr": "paramètres node/upid manquants", "de": "Parameter node/upid fehlen",
		},
		"请求体须含 readonly 布尔": {
			"zh-HK": "請求體須含 readonly 布林", "en": "request body must contain the readonly boolean", "ja": "リクエストボディに readonly ブール値が必要です",
			"ko": "요청 본문에 readonly 불리언이 포함되어야 합니다", "es": "el cuerpo de la petición debe contener el booleano readonly", "fr": "le corps de la requête doit contenir le booléen readonly", "de": "Anfragekörper muss den Boolean readonly enthalten",
		},
	})
	registerPrefix(map[string]map[string]string{
		"PVE API 请求失败:": {
			"zh-HK": "PVE API 請求失敗:", "en": "PVE API request failed: ", "ja": "PVE API リクエストに失敗しました:",
			"ko": "PVE API 요청 실패: ", "es": "fallo de petición a la API PVE: ", "fr": "échec de requête API PVE : ", "de": "PVE-API-Anfrage fehlgeschlagen: ",
		},
	})
}
