export default {
  // ─── jobConfigSchema · shared option sets ──────────────────────────────
  artifactImage: "Container-Image (image)",
  artifactJar: "JAR-Paket (jar)",
  artifactDist: "Statische Assets (dist)",
  buildModelDockerfile: "Eigenes Dockerfile",
  buildModelToolchain: "Plattform-Toolchain",
  toolchainCustom: "Benutzerdefiniert",
  deployStrategyRolling: "Rollierendes Update (ohne Ausfallzeit)",
  deployStrategyRecreate: "Erst stoppen, dann starten (recreate)",
  deployStrategyBlueGreen: "Blau-Grün-Bereitstellung",
  probeModeHttp: "HTTP-Probe",
  probeModeCommand: "Befehls-Probe",
  deployArtifactAuto: "Automatisch (Datei-Artefakt bevorzugen, sonst Image)",
  deployArtifactArchive: "Archivpaket (archive)",

  // ─── jobConfigSchema · exec option fields ──────────────────────────────
  fieldTimeoutLabel: "Timeout (s)",
  fieldTimeoutHint:
    "Ausführungs-Timeout dieses Schritts; bei Überschreitung wird der Container beendet und der Schritt schlägt fehl. Leer / 0 = unbegrenzt",
  fieldRetriesLabel: "Wiederholungen bei Fehlschlag",
  fieldRetriesHint:
    "Bei Exit ungleich null erneut ausführen; schlägt erst beim Erreichen des Limits fehl; Abbruch durch den Benutzer wird nicht wiederholt. Leer / 0 = keine Wiederholung",
  fieldCpuLabel: "CPU-Limit",
  fieldCpuHint:
    "Wird als docker --cpus durchgereicht (z. B. 1, 0.5); leer = unbegrenzt. Abhängig von der docker-Unterstützung des Remote-Runners",
  fieldMemoryLabel: "Speicherlimit",
  fieldMemoryHint:
    "Wird als docker --memory durchgereicht (z. B. 512m, 2g); leer = unbegrenzt",

  // ─── jobConfigSchema · script fields ───────────────────────────────────
  fieldImageLabel: "Laufzeit-Image",
  fieldImageHint:
    "Befehle in diesem isolierten Container ausführen (entspricht einem Jenkins-Agent / Yunxiao-Build-Image)",
  fieldCommandsLabel: "Ausführungsbefehle",
  fieldCommandsHint: "Ein Befehl pro Zeile, der Reihe nach ausgeführt",
  fieldWorkDirLabel: "Arbeitsverzeichnis",
  fieldWorkDirHint:
    "Relativ zum Wurzelverzeichnis des geklonten Workspaces; leer = Workspace-Wurzel",
  fieldArtifactPathLabel: "Artefaktpfade",
  fieldArtifactPathHint:
    "Optional, einer pro Zeile. Relativ zur Workspace-Wurzel: Verzeichnis→dist, *.jar→jar, andere Dateien→archive. Ein Knoten kann mehrere ausgeben; nur wenn gesetzt, werden sie im Artefaktspeicher archiviert und in den Ausführungsdetails herunterladbar/bereitstellbar. Für Image-Artefakte den „Build“-Knoten (build_image) verwenden, nicht hier",
  fieldCachePathsLabel: "Abhängigkeits-Cache-Verzeichnisse",
  fieldCachePathsHint:
    "Optional, eines pro Zeile, relativ zur Workspace-Wurzel. Nur wenn gesetzt, wird das Caching aktiviert: vor dem Build wiederhergestellt, danach gespeichert, Abhängigkeiten werden über mehrere Läufe hinweg wiederverwendet (vermeidet erneutes Herunterladen von node_modules/.m2/.gradle usw.). Cache-Probleme beeinflussen das Build-Ergebnis niemals",
  fieldCacheKeyLabel: "Cache-Key (optional)",
  fieldCacheKeyPlaceholder: "Leer = automatisch nach Branch + Lockfile",
  fieldCacheKeyHint:
    "Optionale Cache-Key-Vorlage. Wenn leer, automatisch aus „Branch + Lockfile-Inhalt im Workspace (package-lock/pom.xml/go.sum usw.)“ abgeleitet: Änderungen an Abhängigkeiten lösen automatisch einen Cold-Build aus. Unterstützt {'{{Parameter}}'}",

  // ─── jobConfigSchema · deploy ssh fields ───────────────────────────────
  fieldDeploySelectorLabel: "Ziel-Selektor",
  fieldDeploySelectorHint: 'Pick label terms to select target hosts (combine with AND/OR); takes precedence over the server picker, empty = skip deploy',
  fieldServerIdLabel: "Zielserver",
  fieldServerIdHint:
    "Einen registrierten Server auswählen (Anmeldedaten per Referenz gebunden)",
  fieldArtifactTypeLabel: "Bereitzustellender Artefakttyp",
  fieldArtifactTypeHint:
    "Welches gewählt wird, wenn dieser Lauf sowohl ein Image als auch Datei-Artefakte ausgibt; ein Image geht per docker pull auf dem Ziel → neuen Container starten → Health-Check → bei Fehler Rollback auf das vorherige Image",
  fieldDeployPathLabel: "Bereitstellungspfad",
  fieldDeployPathHint:
    "Datei-Artefakte: veröffentlicht unter <Bereitstellungspfad>/releases/<runId>/, der current-Symlink wechselt atomar auf diese Version (ohne Ausfallzeit, alte Versionen bleiben für Rollback erhalten)",
  fieldContainerNameLabel: "Containername",
  fieldContainerNameHint:
    "Zielcontainername für Image-Bereitstellungen; leer = Artefaktname verwenden. Ein alter Container mit demselben Namen wird ersetzt und kann auf das vorherige Image zurückgesetzt werden",
  fieldPortsLabel: "Port-Mapping",
  fieldPortsHint:
    "Image-Bereitstellung: durch Komma / Leerzeichen getrennt, jedes Element wird zu docker run -p <map> erweitert",
  fieldRunArgsLabel: "docker-run-Argumente",
  fieldRunArgsHint:
    "Image-Bereitstellung: unverändert an docker run angehängt (beliebige Argumente; für Anmeldedaten den Registry-Login nutzen, nicht hier eintragen)",
  fieldStrategyLabel: "Bereitstellungsstrategie",
  fieldRestartCommandLabel: "Neustart-/Umschaltbefehl",
  fieldRestartCommandHint:
    "Optional, mehrzeilig (set -e, Zeile für Zeile); nach der Bereitstellung im current-Verzeichnis des Ziels ausgeführt, für Neustart/Reload; bei Fehler automatisches Rollback",

  // ─── jobConfigSchema · git_source ──────────────────────────────────────
  typeGitSourceLabel: "Quellcode abrufen",
  typeGitSourceDesc: "Klont ein Git-Repository in den Build-Workspace",
  fieldRepoUrlLabel: "Repository-URL",
  fieldRepoUrlHint:
    "Leer = das mit dem Projekt verknüpfte Standard-Repository verwenden",
  fieldBranchLabel: "Branch / Ref",
  fieldBranchHint:
    "Leer = den Trigger-Branch oder den Standard-Branch des Projekts verwenden",
  fieldCredentialIdLabel: "Zugriffsanmeldedaten",
  fieldCredentialIdHint:
    "Private Repositories benötigen eine referenzierte Git-Token-Anmeldung",
  fieldDepthLabel: "Klontiefe",
  fieldDepthHint: "Tiefe des flachen Klons; leer = vollständige Historie",

  // ─── jobConfigSchema · build_image ─────────────────────────────────────
  typeBuildImageLabel: "Build",
  typeBuildImageDesc: "Erstellt ein Artefakt (Image / JAR / statische Assets)",
  fieldArtifactTypeBuildLabel: "Artefakttyp",
  fieldBuildModelLabel: "Build-Methode",
  fieldBuildModelHint:
    "Eigenes Dockerfile oder von der Plattform per Toolchain bauen lassen",
  fieldDockerfilePathLabel: "Dockerfile-Pfad",
  fieldContextLabel: "Build-Kontext",
  fieldContextHint: "Build-Kontextverzeichnis relativ zur Repository-Wurzel",
  fieldToolchainLanguageLabel: "Toolchain-Sprache",
  fieldToolchainVersionLabel: "Toolchain-Version",
  fieldBuildCommandLabel: "Build-Befehl",
  fieldBuildCommandHint: "Build-Einstiegsbefehl für die Toolchain-Methode",

  // ─── jobConfigSchema · push_image ──────────────────────────────────────
  typePushImageLabel: "Image pushen",
  typePushImageDesc: "Pusht das Image in eine Container-Registry",
  fieldRegistryLabel: "Image-Registry-URL",
  fieldImageNameLabel: "Image-Name",
  fieldTagLabel: "Tag",
  fieldTagHint:
    "Verfügbare Variablen: {'${COMMIT_SHA}'} {'${BRANCH}'} {'${BUILD_NUMBER}'}",
  fieldRegistryCredentialLabel: "Registry-Anmeldedaten",
  fieldRegistryCredentialHint:
    "Referenziert die Login-Anmeldedaten der Image-Registry",
  fieldRegistryUrlLabel: "Image-Registry-URL",
  fieldRegistryUrlHint:
    "Leer = Anmeldung bei Docker Hub; für eine private Registry host[:port] angeben (ohne https://), passend zur Image-Referenz",

  // ─── jobConfigSchema · deploy_ssh ──────────────────────────────────────
  typeDeploySshLabel: 'Host deploy',
  typeDeploySshDesc: 'Roll out file artifacts to target hosts over SSH: releases/<runId> dir + atomic current symlink + restart command + health gate + auto-rollback; supports batched rolling.',

  // ─── jobConfigSchema · health_check ────────────────────────────────────
  typeHealthCheckLabel: "Health-Gate",
  typeHealthCheckDesc:
    "Prüft nach der Bereitstellung die Gesundheit, Rollback bei Fehler",
  fieldProbeModeLabel: "Probe-Modus",
  fieldUrlLabel: "Probe-URL",
  fieldExpectStatusLabel: "Erwarteter Statuscode",
  fieldCommandLabel: "Probe-Befehl",
  fieldHealthRetriesLabel: "Anzahl Wiederholungen",
  fieldIntervalSecondsLabel: "Wiederholungsintervall (s)",

  // ─── jobConfigSchema · notify ──────────────────────────────────────────
  typeNotifyLabel: "Benachrichtigen",
  typeNotifyDesc:
    "Sendet eine Benachrichtigung, wenn der Lauf diesen Knoten erreicht",
  fieldChannelLabel: "Benachrichtigungskanal",
  fieldChannelHint:
    "Wähle einen bereits unter „Benachrichtigungen“ konfigurierten Kanal; falls keiner vorhanden ist, füge zuerst einen in den Einstellungen hinzu",
  fieldTitleTemplateLabel: "Titelvorlage (optional)",
  fieldTitleTemplatePlaceholder: "Bereitstellung erfolgreich: {'{{project}}'}",
  fieldTitleTemplateHint:
    "Leer = Standardtext; unterstützt Platzhalter {'{{project}}'} {'{{branch}}'} {'{{commit}}'} {'{{status}}'} {'{{runId}}'}",
  fieldBodyTemplateLabel: "Textvorlage (optional)",
  fieldBodyTemplatePlaceholder:
    "Branch {'{{branch}}'} {'@'} {'{{commit}}'} hat den Benachrichtigungsknoten erreicht.",
  fieldBodyTemplateHint:
    "Leer = Standardtext; unterstützt ebenfalls {'{{Platzhalter}}'}, unbekannte werden leer gerendert",

  // ─── jobConfigSchema · template nodes ──────────────────────────────────
  typeBuildFrontendLabel: "Frontend-Build",
  typeBuildFrontendDesc:
    "Abhängigkeiten installieren + bauen in einem Node-Container, gibt dist aus",
  typeBuildBackendLabel: "Backend-Build",
  typeBuildBackendDesc:
    "In einem Maven/Gradle-Container paketieren, gibt jar aus",
  typeDeployFrontendLabel: "Frontend-Push-Bereitstellung",
  typeDeployFrontendDesc:
    "Stellt das Frontend-dist per SSH ohne Ausfallzeit auf einem Server bereit",
  typeTemplatedLabel: "Benutzerdefinierter Knoten",
  typeTemplatedDesc:
    "Definiere eigene Parameter + Befehlsvorlage ({'{{Parameter}}'}), durch Setzen der Parameter sofort nutzbar",
  fieldParamsLabel: "Parametertabelle",
  fieldParamsHint:
    "Ein key=value pro Zeile, völlig frei; in der Befehlsvorlage / den Artefaktpfaden mit {'{{key}}'} referenzieren",
  fieldCommandTemplateLabel: "Befehlsvorlage",
  fieldCommandTemplateHint:
    "Mehrzeilig; {'{{Parameter}}'} wird durch den Wert der Parametertabelle ersetzt, $ENV wird weiterhin von der Shell im Container verarbeitet",
  fieldTemplatedArtifactPathHint:
    "Optional, einer pro Zeile, unterstützt {'{{Parameter}}'} und Wildcards; Verzeichnis→dist, *.jar→jar, andere→archive",
  fieldTemplatedWorkDirHint:
    "Optional, relativ zur Wurzel des geklonten Workspaces",
  fieldTemplatedCachePathsHint:
    "Optional, eines pro Zeile, relativ zur Workspace-Wurzel, unterstützt {'{{Parameter}}'}. Nur wenn gesetzt, wird das Caching aktiviert: vor dem Build wiederhergestellt, danach gespeichert, Abhängigkeiten werden über Läufe hinweg wiederverwendet. Cache-Probleme beeinflussen das Build-Ergebnis niemals",
  fieldTemplatedCacheKeyHint:
    "Optionale Cache-Key-Vorlage, unterstützt {'{{Parameter}}'}. Wenn leer, automatisch aus Branch + Lockfile-Inhalt abgeleitet (Änderungen an Abhängigkeiten lösen einen Cold-Build aus)",
  typeScriptLabel: "Benutzerdefiniertes Skript",
  typeScriptDesc: "Führt beliebige Befehle in einem isolierten Container aus",

  // ─── jobConfigSchema · categories ──────────────────────────────────────
  categorySource: "Quelle",
  categoryBuild: "Build & Artefakte",
  categoryDeploy: "Bereitstellung",
  categoryQuality: "Qualitäts-Gates",
  categoryNotify: "Benachrichtigen",
  categoryCustom: "Benutzerdefiniert",

  // ─── stepCompile · step kind meta ──────────────────────────────────────
  stepCommandLabel: "Befehl ausführen",
  stepEnvLabel: "Umgebungsvariable setzen",
  stepWorkDirLabel: "Verzeichnis wechseln",
  stepArtifactLabel: "Artefakt hochladen",
  stepConditionLabel: "Bedingungs-Guard",

  // ─── studioCompile · catalog groups + items ────────────────────────────
  studioGroupBasic: "Grundlagen",
  studioGroupEnvDir: "Umgebung & Verzeichnis",
  studioGroupQuality: "Qualitäts-Gates",
  studioGroupArtifact: "Artefakte",
  studioGroupControl: "Kontrollfluss",
  studioGroupDoc: "Dokumente",
  studioStepCommand: "Befehl ausführen",
  studioStepInstall: "Abhängigkeiten installieren",
  studioStepEcho: "Log ausgeben",
  studioStepEnv: "Umgebungsvariable setzen",
  studioStepWorkDir: "Verzeichnis wechseln",
  studioStepPath: "PATH hinzufügen",
  studioStepTest: "Tests ausführen + Bericht",
  studioStepHealthcheck: "Health-Check",
  studioStepArtifact: "Artefakt hochladen",
  studioStepDownload: "Datei herunterladen",
  studioStepExtract: "Archiv entpacken",
  studioStepCondition: "Bedingungs-Guard",
  studioStepRetry: "Wiederholungs-Wrapper",
  studioStepTimeout: "Timeout-Grenze",
  studioStepSleep: "Warten / Verzögerung",
  studioStepNote: "Notiz",
  studioDefaultCategory: "Benutzerdefiniert",

  // ─── stageSettings · when events ───────────────────────────────────────
  whenManual: "Manuell",
  whenSchedule: "Geplant",

  // ─── stageSettings · post conditions ───────────────────────────────────
  postAlways: "Immer",
  postOnSuccess: "Bei Erfolg",
  postOnFailure: "Bei Fehler",

  // ─── stageSettings · matrix errors ─────────────────────────────────────
  matrixAxisInvalid:
    "Achsenname „{name}“ ist ungültig (muss ein Bezeichner sein: beginnt mit Buchstabe/Unterstrich)",
  matrixAxisNeedsValue: "Achse „{name}“ benötigt mindestens einen Wert",
  matrixTooManyAxes: "Matrixdimension {count} überschreitet das Limit {max}",
  matrixTooManyCells:
    "Die Matrix expandiert auf {cells} Zellen und überschreitet das Limit {max}",

  // ─── JobDrawer.vue ─────────────────────────────────────────────────────
  drawerAria: "Job-Konfiguration",
  unnamedJob: "(unbenannter Job)",
  thisPipeline: "diese Pipeline",
  closeDrawer: "Panel schließen",
  basicInfo: "Grundinformationen",
  jobNameLabel: "Jobname",
  jobNamePlaceholder: "z. B.: Isolierter Build",
  jobTypeLabel: "Jobtyp",
  changeType: "Jobtyp ändern",
  change: "Ändern",
  summaryLabel: "Zusammenfassung",
  summaryPlaceholder: "Karten-Untertitel (optional)",
  instanceParams: "Instanzparameter",
  instanceParamsHint:
    "Dieser benutzerdefinierte Knoten stellt die folgenden Parameter bereit; passe sie nach Bedarf an, der Rest der Befehlsvorlage ist vom Knotenautor festgelegt.",
  advancedRawToggle: "Erweitert · Rohparameter anzeigen/bearbeiten",
  configSuffix: "-Konfiguration",
  configView: "Konfigurationsansicht",
  visualSteps: "Visuelle Schritte",
  rawParams: "Rohparameter",
  execAdvancedToggle: "Erweitert · Timeout / Wiederholung / Ressourcen / Cache",
  toggleEnable: "Aktivieren",
  stepBuilderHint:
    "Schritte werden zu mehrzeiligen Befehlen kompiliert, die in einem isolierten Container ausgeführt werden; Verzeichnis wechseln / Umgebungsvariable setzen wirkt sich auf nachfolgende Befehle aus. Du kannst jederzeit zu „Rohparameter“ wechseln, um sie anzusehen.",
  advancedRawParams: "Erweitert · Rohparameter",
  kvEmptyWithSpec:
    "Die Parameter dieses Typs befinden sich im obigen Formular; hier kannst du benutzerdefinierte Schlüssel-Werte hinzufügen",
  kvEmptyNoSpec:
    "Benutzerdefinierte Schlüssel-Wert-Parameter für diesen Typ hinzufügen",
  kvKeyPlaceholder: "key",
  kvValuePlaceholder: "value",
  kvKeyAria: "Konfigurationsschlüssel {n}",
  kvValueAria: "Konfigurationswert {n}",
  kvDelAria: "Konfigurationselement {label} löschen",
  addParam: "Parameter hinzufügen",
  credUnselected: "— Nicht ausgewählt —",
  channelUnselected: "— Kanal auswählen —",
  channelDisabled: "(deaktiviert)",
  saveAsCustomNode: "Als benutzerdefinierten Knoten speichern",
  savedToLibrary: "✓ In der Wiederverwendungsbibliothek gespeichert",
  savePanelHint:
    "Speichert einen Schnappschuss des Typs und der Parameter dieses Knotens in der Wiederverwendungsbibliothek, um ihn später in jeder Pipeline auswählen zu können.",
  saveNamePlaceholder: "Knotenname (in der Bibliothek eindeutig)",
  saveNameAria: "Name des benutzerdefinierten Knotens",
  saveDescPlaceholder: "Beschreibung (optional)",
  saveDescAria: "Beschreibung des benutzerdefinierten Knotens",
  cancel: "Abbrechen",
  saving: "Speichern…",
  save: "Speichern",
  saveErrEmptyName: "Bitte einen Knotennamen eingeben",
  saveErrFailed:
    "Speichern fehlgeschlagen, bitte erneut versuchen (möglicherweise doppelter Name)",
  channelTypeEmail: "E-Mail",
  channelTypeFeishu: "Feishu",
  channelTypeWecom: "WeCom",
  channelTypeDingtalk: "DingTalk",

  // ─── StepBuilder.vue ───────────────────────────────────────────────────
  sbVisualSteps: "Visuelle Schritte",
  sbStepCount: "Anzahl Schritte",
  sbDragSort: "Zum Sortieren ziehen",
  sbMoveUp: "Schritt {n} nach oben",
  sbMoveDown: "Schritt {n} nach unten",
  sbDelStep: "Schritt {n} löschen",
  sbCommandAria: "Befehl {n}",
  sbEnvKeyAria: "Name der Umgebungsvariable {n}",
  sbEnvValueAria: "Wert der Umgebungsvariable {n}",
  sbDirAria: "Verzeichnis {n}",
  sbCondAria: "Bedingung {n}",
  sbCondHintPre: "Shell-Bedingung; wenn nicht erfüllt, ",
  sbCondHintStrong: "alle nachfolgenden Schritte überspringen",
  sbCondHintPost: " (erfolgreich beenden). z. B.",
  sbArtifactAria: "Artefaktpfad {n}",
  sbEmpty:
    "Noch keine Schritte, klicke unten auf „Schritt hinzufügen“, um zu beginnen",
  sbAddStep: "Schritt hinzufügen",
  sbAddCommandDesc: "Shell-Befehl (mehrzeilig)",
  sbAddEnvDesc: "KEY=VALUE in die Umgebung injizieren",
  sbAddWorkDirDesc: "Arbeitsverzeichnis für nachfolgende Befehle wechseln",
  sbAddConditionDesc: "nachfolgende Schritte überspringen, wenn nicht erfüllt",
  sbAddArtifactDesc: "glob zum Archivieren von Artefakten",
  typeBuildNodejsLabel: 'Node.js build',
  typeBuildNodejsDesc: 'Install deps and build inside a node container, producing dist. Great for package.json projects.',
  typeBuildJavaLabel: 'Java build',
  typeBuildJavaDesc: 'Package inside a Maven container, producing a jar (Gradle: just swap image and command).',
  typeBuildGolangLabel: 'Golang build',
  typeBuildGolangDesc: 'Compile inside a golang container, producing a binary.',
  typeBuildPythonLabel: 'Python build',
  typeBuildPythonDesc: 'Install deps and build inside a python container, producing wheels/sdists.',
  typeDeployContainerLabel: 'Container deploy',
  typeDeployContainerDesc: 'Deploy an image on target hosts: pull → remove old container → run new one → health gate → rollback to previous image on failure.',
  deployModeArtifact: 'Deploy artifact',
  deployModeCommand: 'Restart command only',
  fieldDeployModeLabel: 'Deploy content',
  fieldDeployModeHint: 'Deploy the selected artifact; or touch no artifact and only run the restart command on targets.',
  fieldArtifactJobLabel: 'Artifact source',
  fieldArtifactJobHint: 'Pick exactly which node produced the artifact to deploy (explicit binding, no auto guessing).',
  fieldArtifactJobImageHint: 'Pick a build_image node that produces the image.',
  fieldArtifactNameLabel: 'Artifact name filter',
  fieldArtifactNameHint: 'Glob to disambiguate when the source node emits multiple artifacts (e.g. app-*.jar).',
  fieldFirstBatchSizeLabel: 'First batch size',
  fieldFirstBatchSizeHint: 'Hosts in the first rolling batch (default 1 — verify small first).',
  fieldBatchSizeLabel: 'Batch size',
  fieldBatchSizeHint: 'Hosts upgraded simultaneously per batch after the first (empty/0 = all remaining at once; any batch failure stops the rollout).',
  fieldHealthUrlLabel: 'Health check URL',
  fieldHealthUrlHint: 'curl-probed on the target after deploy; failure triggers rollback. Empty = no gate.',
  fieldHealthCommandLabel: 'Health check command',
  fieldHealthCommandHint: 'Command probe (ignored when URL is set).',
  fieldHealthRetriesHint: 'Probe retries (default 3).',
  fieldHealthIntervalLabel: 'Probe interval (s)',
  fieldHealthIntervalHint: 'Seconds between probes (default 3).',
  fieldSelectorModeLabel: 'Label matching',
  fieldSelectorModeHint: 'How multiple label terms combine: match all (AND) or match any (OR).',
  selectorModeAll: 'Match all terms (AND)',
  selectorModeAny: 'Match any term (OR)',
  fieldArtifactPackModeLabel: 'Artifact packing',
  fieldArtifactPackModeHint: 'Pack directories into a tar.gz (default); or store as a file manifest without packing.',
  artifactPackTar: 'Pack as tar.gz',
  artifactPackNone: 'No packing (file manifest)',
  fieldTarLayoutLabel: 'Archive layout',
  fieldTarLayoutHint: 'Contents at archive root (unpacks into the release dir) = legacy; or keep the top-level directory.',
  tarLayoutContents: 'Contents at root (no dir prefix)',
  tarLayoutTop: 'Keep top-level directory',
  fieldArtifactRenameLabel: 'Artifact file name',
  fieldArtifactRenameHint: 'Optional: file name on the target host for single-file artifacts (default: original).',
  fieldHealthSelectorHint: 'Pick label terms to select hosts to probe; empty + http probe falls back to the platform host.',
  selectorNoServers: 'No servers yet — add one in Server management first',
  selectorNoLabels: 'No labels yet — label your servers in Server management first',
  selectorKeyAria: 'Label key',
  selectorValueAria: 'Label value',
  selectorDelAria: 'Remove label term {k}',
  selectorAddTerm: 'Add label term',
  producerUnselected: 'Select the artifact source node',
};
