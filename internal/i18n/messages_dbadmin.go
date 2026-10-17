package i18n

// 数据库管理(MySQL/PostgreSQL 实例注册 + phpMyAdmin 式在线管理)的 API 消息。
// 密码/SQL 文本绝不入消息;动态 detail(驱动错误等)经前缀词条透传。
func init() {
	register(map[string]map[string]string{
		"数据库实例不存在": {
			"zh-HK": "資料庫實例不存在", "en": "database instance not found", "ja": "データベースインスタンスが存在しません",
			"ko": "데이터베이스 인스턴스가 없습니다", "es": "instancia de base de datos no encontrada", "fr": "instance de base de données introuvable", "de": "Datenbankinstanz nicht gefunden",
		},
		"实例处于只读模式,请先解锁写模式": {
			"zh-HK": "實例處於唯讀模式,請先解鎖寫入模式", "en": "instance is read-only; unlock write mode first", "ja": "インスタンスは読み取り専用です。先に書き込みモードを解除してください",
			"ko": "인스턴스가 읽기 전용입니다. 먼저 쓰기 모드를 잠금 해제하세요", "es": "la instancia está en modo de solo lectura; desbloquee primero el modo de escritura", "fr": "l'instance est en lecture seule ; déverrouillez d'abord le mode écriture", "de": "Instanz ist im Nur-Lese-Modus; entsperren Sie zuerst den Schreibmodus",
		},
		"引擎仅支持 mysql / postgres": {
			"zh-HK": "引擎僅支援 mysql / postgres", "en": "engine must be mysql or postgres", "ja": "エンジンは mysql / postgres のみサポートされます",
			"ko": "엔진은 mysql / postgres만 지원됩니다", "es": "el motor solo admite mysql / postgres", "fr": "le moteur ne prend en charge que mysql / postgres", "de": "Als Engine sind nur mysql / postgres unterstützt",
		},
		"实例名称不能为空": {
			"zh-HK": "實例名稱不能為空", "en": "instance name must not be empty", "ja": "インスタンス名は空にできません",
			"ko": "인스턴스 이름은 비워 둘 수 없습니다", "es": "el nombre de la instancia no debe estar vacío", "fr": "le nom de l'instance ne doit pas être vide", "de": "Instanzname darf nicht leer sein",
		},
		"用户名不能为空": {
			"zh-HK": "使用者名稱不能為空", "en": "username must not be empty", "ja": "ユーザー名は空にできません",
			"ko": "사용자 이름은 비워 둘 수 없습니다", "es": "el nombre de usuario no debe estar vacío", "fr": "le nom d'utilisateur ne doit pas être vide", "de": "Benutzername darf nicht leer sein",
		},
		"密码不能为空": {
			"zh-HK": "密碼不能為空", "en": "password must not be empty", "ja": "パスワードは空にできません",
			"ko": "비밀번호는 비워 둘 수 없습니다", "es": "la contraseña no debe estar vacía", "fr": "le mot de passe ne doit pas être vide", "de": "Passwort darf nicht leer sein",
		},
		"端口须在 1-65535 之间": {
			"zh-HK": "連接埠須在 1-65535 之間", "en": "port must be between 1 and 65535", "ja": "ポートは 1~65535 の範囲で指定してください",
			"ko": "포트는 1-65535 사이여야 합니다", "es": "el puerto debe estar entre 1 y 65535", "fr": "le port doit être compris entre 1 et 65535", "de": "Port muss zwischen 1 und 65535 liegen",
		},
		"同引擎同地址的实例已存在": {
			"zh-HK": "同引擎同位址的實例已存在", "en": "an instance with the same engine and address already exists", "ja": "同じエンジン・同じアドレスのインスタンスが既に存在します",
			"ko": "동일한 엔진과 주소의 인스턴스가 이미 있습니다", "es": "ya existe una instancia con el mismo motor y dirección", "fr": "une instance avec le même moteur et la même adresse existe déjà", "de": "Eine Instanz mit derselben Engine und Adresse existiert bereits",
		},
		"保险库未配置 master key,无法保存数据库密码": {
			"zh-HK": "保險庫未設定 master key,無法保存資料庫密碼", "en": "vault has no master key configured; cannot store database passwords", "ja": "vault に master key が設定されていないため、データベースのパスワードを保存できません",
			"ko": "vault에 master key가 구성되지 않아 데이터베이스 비밀번호를 저장할 수 없습니다", "es": "la bóveda no tiene master key configurada; no se pueden guardar contraseñas de bases de datos", "fr": "le coffre-fort n'a pas de master key configurée ; impossible d'enregistrer les mots de passe de base de données", "de": "Vault hat keinen master key konfiguriert; Datenbankpasswörter können nicht gespeichert werden",
		},
		"库/表/列标识符非法": {
			"zh-HK": "庫/表/欄位識別碼非法", "en": "invalid database/table/column identifier", "ja": "データベース/テーブル/カラムの識別子が不正です",
			"ko": "데이터베이스/테이블/컬럼 식별자가 잘못되었습니다", "es": "identificador de base de datos/tabla/columna no válido", "fr": "identifiant de base de données/table/colonne non valide", "de": "ungültiger Datenbank-/Tabellen-/Spaltenbezeichner",
		},
		"表没有主键,无法定位行": {
			"zh-HK": "表沒有主鍵,無法定位行", "en": "table has no primary key; cannot address rows", "ja": "テーブルに主キーがないため、行を特定できません",
			"ko": "테이블에 기본 키가 없어 행을 특정할 수 없습니다", "es": "la tabla no tiene clave primaria; no se pueden direccionar filas", "fr": "la table n'a pas de clé primaire ; impossible d'adresser les lignes", "de": "Tabelle hat keinen Primärschlüssel; Zeilen können nicht adressiert werden",
		},
		"生效参数缺少密码/端口,未自动登记数据库实例": {
			"zh-HK": "生效參數缺少密碼/連接埠,未自動登記資料庫實例", "en": "effective params lack password/port; database instance not auto-registered", "ja": "有効パラメータにパスワード/ポートがないため、データベースインスタンスを自動登録しませんでした",
			"ko": "유효 매개변수에 비밀번호/포트가 없어 데이터베이스 인스턴스를 자동 등록하지 않았습니다", "es": "faltan contraseña/puerto en los parámetros efectivos; instancia de base de datos no registrada automáticamente", "fr": "mot de passe/port manquants dans les paramètres effectifs ; instance de base de données non enregistrée automatiquement", "de": "effektive Parameter ohne Passwort/Port; Datenbankinstanz nicht automatisch registriert",
		},
		"生效端口参数非法,未自动登记数据库实例": {
			"zh-HK": "生效連接埠參數非法,未自動登記資料庫實例", "en": "invalid effective port param; database instance not auto-registered", "ja": "有効なポートパラメータが不正なため、データベースインスタンスを自動登録しませんでした",
			"ko": "유효 포트 매개변수가 잘못되어 데이터베이스 인스턴스를 자동 등록하지 않았습니다", "es": "parámetro de puerto efectivo no válido; instancia de base de datos no registrada automáticamente", "fr": "paramètre de port effectif non valide ; instance de base de données non enregistrée automatiquement", "de": "ungültiger effektiver Port-Parameter; Datenbankinstanz nicht automatisch registriert",
		},
		"自动登记数据库实例失败": {
			"zh-HK": "自動登記資料庫實例失敗", "en": "failed to auto-register database instance", "ja": "データベースインスタンスの自動登録に失敗しました",
			"ko": "데이터베이스 인스턴스 자동 등록에 실패했습니다", "es": "no se pudo registrar automáticamente la instancia de base de datos", "fr": "échec de l'enregistrement automatique de l'instance de base de données", "de": "automatische Registrierung der Datenbankinstanz fehlgeschlagen",
		},
		"已自动登记到「设置 → 数据库」,可在「数据库」页在线管理": {
			"zh-HK": "已自動登記到「設定 → 資料庫」,可在「資料庫」頁線上管理", "en": "auto-registered under Settings → Databases; manage it on the Databases page", "ja": "「設定 → データベース」に自動登録しました。「データベース」ページで管理できます",
			"ko": "「설정 → 데이터베이스」에 자동 등록되었습니다. 「데이터베이스」 페이지에서 관리할 수 있습니다", "es": "registrada automáticamente en Ajustes → Bases de datos; gestione en la página Bases de datos", "fr": "enregistrée automatiquement dans Paramètres → Bases de données ; à gérer depuis la page Bases de données", "de": "automatisch unter Einstellungen → Datenbanken registriert; auf der Seite Datenbanken verwaltbar",
		},
		"已更新既有数据库实例的密码(同地址原地轮换)": {
			"zh-HK": "已更新既有資料庫實例的密碼(同位址原地輪換)", "en": "rotated the existing database instance's password (same address, in place)", "ja": "既存のデータベースインスタンスのパスワードを更新しました(同一アドレスでその場で更新)",
			"ko": "기존 데이터베이스 인스턴스의 비밀번호를 갱신했습니다(동일 주소에서 제자리 교체)", "es": "se rotó la contraseña de la instancia existente (misma dirección, en el sitio)", "fr": "mot de passe de l'instance existante renouvelé (même adresse, sur place)", "de": "Passwort der bestehenden Instanz rotiert (gleiche Adresse, vor Ort)",
		},
	})
	registerPrefix(map[string]map[string]string{
		"连接失败:": {
			"zh-HK": "連線失敗:", "en": "connection failed: ", "ja": "接続に失敗しました:",
			"ko": "연결 실패: ", "es": "fallo de conexión: ", "fr": "échec de connexion : ", "de": "Verbindung fehlgeschlagen: ",
		},
		"入参非法:": {
			"zh-HK": "入參非法:", "en": "invalid input: ", "ja": "入力が不正です:",
			"ko": "입력이 잘못되었습니다: ", "es": "entrada no válida: ", "fr": "entrée non valide : ", "de": "ungültige Eingabe: ",
		},
	})
}
