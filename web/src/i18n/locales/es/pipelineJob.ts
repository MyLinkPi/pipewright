export default {
  // ─── jobConfigSchema · shared option sets ──────────────────────────────
  artifactImage: "Imagen de contenedor (image)",
  artifactJar: "Paquete JAR (jar)",
  artifactDist: "Recursos estáticos (dist)",
  buildModelDockerfile: "Dockerfile propio",
  buildModelToolchain: "Cadena de herramientas de la plataforma",
  toolchainCustom: "Personalizado",
  deployStrategyRolling: "Actualización progresiva (sin tiempo de inactividad)",
  deployStrategyRecreate: "Detener y luego iniciar (recreate)",
  deployStrategyBlueGreen: "Despliegue azul-verde",
  probeModeHttp: "Sonda HTTP",
  probeModeCommand: "Sonda por comando",
  deployArtifactAuto:
    "Automático (preferir artefacto de archivo, si no usar imagen)",
  deployArtifactArchive: "Paquete de archivo (archive)",

  // ─── jobConfigSchema · exec option fields ──────────────────────────────
  fieldTimeoutLabel: "Tiempo de espera (s)",
  fieldTimeoutHint:
    "Tiempo de espera de ejecución de este paso; al superarse se termina el contenedor y se marca como fallido. Vacío / 0 = sin límite",
  fieldRetriesLabel: "Reintentos en caso de fallo",
  fieldRetriesHint:
    "Reejecuta al salir con código distinto de cero; solo falla al alcanzar el límite; la cancelación del usuario no se reintenta. Vacío / 0 = sin reintentos",
  fieldCpuLabel: "Límite de CPU",
  fieldCpuHint:
    "Se pasa como docker --cpus (p. ej. 1, 0.5); vacío = sin límite. Depende del soporte de docker del runner remoto",
  fieldMemoryLabel: "Límite de memoria",
  fieldMemoryHint:
    "Se pasa como docker --memory (p. ej. 512m, 2g); vacío = sin límite",

  // ─── jobConfigSchema · script fields ───────────────────────────────────
  fieldImageLabel: "Imagen de ejecución",
  fieldImageHint:
    "Ejecuta comandos dentro de este contenedor aislado (equivalente a un Jenkins agent / imagen de build de Yunxiao)",
  jobRunnerHint: 'Vacio = sigue el valor por defecto de stage/proyecto (stage en ajustes del nodo, proyecto en la pestana Build). Cada nodo ocupa su propio slot: los nodos con el mismo selector toman una maquina del mismo grupo — en paralelo si hay capacidad, en cola si no; espacios de trabajo independientes.',
  fieldCommandsLabel: "Comandos de ejecución",
  fieldCommandsHint: "Un comando por línea, ejecutados en orden",
  fieldWorkDirLabel: "Directorio de trabajo",
  fieldWorkDirHint:
    "Relativo a la raíz del espacio de trabajo clonado; vacío = raíz del espacio de trabajo",
  fieldArtifactPathLabel: "Rutas de artefactos",
  fieldArtifactPathHint:
    "Opcional, uno por línea. Relativo a la raíz del espacio de trabajo: directorio→dist, *.jar→jar, otros archivos→archive. Un nodo puede emitir varios; solo al definirlos se archivan en el almacén de artefactos y se pueden descargar/desplegar en los detalles de la ejecución. Para artefactos de imagen usa el nodo «Build» (build_image), no aquí",
  fieldCachePathsLabel: "Directorios de caché de dependencias",
  fieldCachePathsHint:
    "Opcional, uno por línea, relativo a la raíz del espacio de trabajo. Solo al definirlos se activa la caché: se restaura antes del build, se guarda después, reutilizando dependencias entre ejecuciones (evita volver a descargar node_modules/.m2/.gradle, etc.). Los problemas de caché nunca afectan al resultado del build",
  fieldCacheKeyLabel: "Clave de caché (opcional)",
  fieldCacheKeyPlaceholder: "Vacío = automático por rama + lockfile",
  fieldCacheKeyHint:
    "Plantilla de clave de caché opcional. Si está vacía, se deriva automáticamente de «rama + contenido del lockfile en el espacio de trabajo (package-lock/pom.xml/go.sum, etc.)»: los cambios de dependencias provocan un build en frío automático. Admite {'{{parámetro}}'}",

  // ─── jobConfigSchema · deploy ssh fields ───────────────────────────────
  fieldDeploySelectorLabel: "Selector de destino",
  fieldDeploySelectorHint: 'Pick label terms to select target hosts (combine with AND/OR); takes precedence over the server picker, empty = skip deploy',
  fieldServerIdLabel: "Servidor de destino",
  fieldServerIdHint:
    "Selecciona un servidor registrado (las credenciales se vinculan por referencia)",
  fieldArtifactTypeLabel: "Tipo de artefacto a desplegar",
  fieldArtifactTypeHint:
    "Cuál elegir cuando esta ejecución emite tanto una imagen como artefactos de archivo; una imagen va mediante docker pull en el destino → iniciar nuevo contenedor → comprobación de salud → revertir a la imagen anterior si falla",
  fieldDeployPathLabel: "Ruta de despliegue",
  fieldDeployPathHint:
    "Artefactos de archivo: se publican en <ruta de despliegue>/releases/<runId>/, el enlace simbólico current cambia de forma atómica a esta versión (sin tiempo de inactividad, las versiones antiguas se conservan para revertir)",
  fieldContainerNameLabel: "Nombre del contenedor",
  fieldContainerNameHint:
    "Nombre del contenedor de destino para despliegues de imagen; vacío = usar el nombre del artefacto. El contenedor antiguo con el mismo nombre se reemplaza y puede revertirse a la imagen anterior",
  fieldPortsLabel: "Mapeo de puertos",
  fieldPortsHint:
    "Despliegue de imagen: separado por comas / espacios, cada elemento se expande a docker run -p <map>",
  fieldRunArgsLabel: "Argumentos de docker run",
  fieldRunArgsHint:
    "Despliegue de imagen: se añaden tal cual a docker run (argumentos libres; para credenciales usa el login del registry, no los pongas aquí)",
  fieldStrategyLabel: "Estrategia de despliegue",
  fieldRestartCommandLabel: "Comando de reinicio / cambio",
  fieldRestartCommandHint:
    "Opcional, multilínea (set -e, línea por línea); se ejecuta en el directorio current del destino tras el despliegue, para reiniciar/recargar; revierte automáticamente si falla",

  // ─── jobConfigSchema · git_source ──────────────────────────────────────
  typeGitSourceLabel: "Obtener código fuente",
  typeGitSourceDesc:
    "Clona un repositorio Git en el espacio de trabajo de build",
  fieldRepoUrlLabel: "URL del repositorio",
  fieldRepoUrlHint:
    "Vacío = usar el repositorio predeterminado vinculado al proyecto",
  fieldBranchLabel: "Rama / Ref",
  fieldBranchHint:
    "Vacío = usar la rama del disparador o la rama predeterminada del proyecto",
  fieldCredentialIdLabel: "Credencial de acceso",
  fieldCredentialIdHint:
    "Los repositorios privados requieren una credencial de token Git referenciada",
  fieldDepthLabel: "Profundidad de clonado",
  fieldDepthHint:
    "Profundidad de clonado superficial; vacío = historial completo",

  // ─── jobConfigSchema · build_image ─────────────────────────────────────
  typeBuildImageLabel: "Build",
  typeBuildImageDesc:
    "Construye un artefacto (imagen / JAR / recursos estáticos)",
  fieldArtifactTypeBuildLabel: "Tipo de artefacto",
  fieldBuildModelLabel: "Método de build",
  fieldBuildModelHint:
    "Dockerfile propio, o que la plataforma construya por cadena de herramientas",
  fieldDockerfilePathLabel: "Ruta del Dockerfile",
  fieldContextLabel: "Contexto de build",
  fieldContextHint:
    "Directorio de contexto de build relativo a la raíz del repositorio",
  fieldToolchainLanguageLabel: "Lenguaje de la cadena de herramientas",
  fieldToolchainVersionLabel: "Versión de la cadena de herramientas",
  fieldBuildCommandLabel: "Comando de build",
  fieldBuildCommandHint:
    "Comando de entrada de build para el método de cadena de herramientas",

  // ─── jobConfigSchema · push_image ──────────────────────────────────────
  typePushImageLabel: "Enviar imagen",
  typePushImageDesc: "Envía la imagen a un registro de contenedores",
  fieldRegistryLabel: "URL del registro de imágenes",
  fieldImageNameLabel: "Nombre de la imagen",
  fieldTagLabel: "Etiqueta",
  fieldTagHint:
    "Variables disponibles: {'${COMMIT_SHA}'} {'${BRANCH}'} {'${BUILD_NUMBER}'}",
  fieldRegistryCredentialLabel: "Credencial del registro",
  fieldRegistryCredentialHint:
    "Referencia la credencial de login del registro de imágenes",
  fieldRegistryUrlLabel: "URL del registro de imágenes",
  fieldRegistryUrlHint:
    "Vacío = iniciar sesión en Docker Hub; en un registro privado indique host[:port] (sin https://), coherente con la referencia de la imagen",

  // ─── jobConfigSchema · deploy_ssh ──────────────────────────────────────
  typeDeploySshLabel: 'Host deploy',
  typeDeploySshDesc: 'Roll out file artifacts to target hosts over SSH: releases/<runId> dir + atomic current symlink + restart command + health gate + auto-rollback; supports batched rolling.',

  // ─── jobConfigSchema · health_check ────────────────────────────────────
  typeHealthCheckLabel: "Puerta de salud",
  typeHealthCheckDesc: "Sondea la salud tras el despliegue, revierte si falla",
  fieldProbeModeLabel: "Modo de sonda",
  fieldUrlLabel: "URL de sonda",
  fieldExpectStatusLabel: "Código de estado esperado",
  fieldCommandLabel: "Comando de sonda",
  fieldHealthRetriesLabel: "Número de reintentos",
  fieldIntervalSecondsLabel: "Intervalo de reintento (s)",

  // ─── jobConfigSchema · notify ──────────────────────────────────────────
  typeNotifyLabel: "Notificar",
  typeNotifyDesc:
    "Envía una notificación cuando la ejecución llega a este nodo",
  fieldChannelLabel: "Canal de notificación",
  fieldChannelHint:
    "Selecciona un canal ya configurado en los ajustes de «Notificaciones»; si no hay ninguno, añade uno primero en los ajustes",
  fieldTitleTemplateLabel: "Plantilla de título (opcional)",
  fieldTitleTemplatePlaceholder: "Despliegue exitoso: {'{{project}}'}",
  fieldTitleTemplateHint:
    "Vacío = texto predeterminado; admite los marcadores {'{{project}}'} {'{{branch}}'} {'{{commit}}'} {'{{status}}'} {'{{runId}}'}",
  fieldBodyTemplateLabel: "Plantilla de cuerpo (opcional)",
  fieldBodyTemplatePlaceholder:
    "La rama {'{{branch}}'} {'@'} {'{{commit}}'} ha llegado al nodo de notificación.",
  fieldBodyTemplateHint:
    "Vacío = texto predeterminado; también admite {'{{marcadores}}'}, los desconocidos se renderizan vacíos",

  // ─── jobConfigSchema · template nodes ──────────────────────────────────
  typeBuildFrontendLabel: "Build de frontend",
  typeBuildFrontendDesc:
    "Instala dependencias + build en un contenedor Node, emite dist",
  typeBuildBackendLabel: "Build de backend",
  typeBuildBackendDesc: "Empaqueta en un contenedor Maven/Gradle, emite jar",
  typeDeployFrontendLabel: "Despliegue push de frontend",
  typeDeployFrontendDesc:
    "Despliega el dist del frontend en un servidor por SSH sin tiempo de inactividad",
  typeTemplatedLabel: "Nodo personalizado",
  typeTemplatedDesc:
    "Define tus propios parámetros + plantilla de comandos ({'{{parámetro}}'}), úsalo configurando los parámetros",
  fieldParamsLabel: "Tabla de parámetros",
  fieldParamsHint:
    "Un key=value por línea, totalmente libre; referéncialo con {'{{key}}'} en la plantilla de comandos / rutas de artefactos",
  fieldCommandTemplateLabel: "Plantilla de comandos",
  fieldCommandTemplateHint:
    "Multilínea; {'{{parámetro}}'} se reemplaza por el valor de la tabla de parámetros, $ENV lo sigue gestionando el shell del contenedor",
  fieldTemplatedArtifactPathHint:
    "Opcional, uno por línea, admite {'{{parámetro}}'} y comodines; directorio→dist, *.jar→jar, otros→archive",
  fieldTemplatedWorkDirHint:
    "Opcional, relativo a la raíz del espacio de trabajo clonado",
  fieldTemplatedCachePathsHint:
    "Opcional, uno por línea, relativo a la raíz del espacio de trabajo, admite {'{{parámetro}}'}. Solo al definirlos se activa la caché: se restaura antes del build, se guarda después, reutilizando dependencias entre ejecuciones. Los problemas de caché nunca afectan al resultado del build",
  fieldTemplatedCacheKeyHint:
    "Plantilla de clave de caché opcional, admite {'{{parámetro}}'}. Si está vacía, se deriva automáticamente de la rama + contenido del lockfile (los cambios de dependencias provocan un build en frío)",
  typeScriptLabel: "Script personalizado",
  typeScriptDesc:
    "Ejecuta comandos arbitrarios dentro de un contenedor aislado",

  // ─── jobConfigSchema · categories ──────────────────────────────────────
  categorySource: "Fuente",
  categoryBuild: "Build y artefactos",
  categoryDeploy: "Despliegue",
  categoryQuality: "Puertas de calidad",
  categoryNotify: "Notificar",
  categoryCustom: "Personalizado",

  // ─── stepCompile · step kind meta ──────────────────────────────────────
  stepCommandLabel: "Ejecutar comando",
  stepEnvLabel: "Definir variable de entorno",
  stepWorkDirLabel: "Cambiar directorio",
  stepArtifactLabel: "Subir artefacto",
  stepConditionLabel: "Guarda condicional",

  // ─── studioCompile · catalog groups + items ────────────────────────────
  studioGroupBasic: "Básicos",
  studioGroupEnvDir: "Entorno y directorio",
  studioGroupQuality: "Puertas de calidad",
  studioGroupArtifact: "Artefactos",
  studioGroupControl: "Flujo de control",
  studioGroupDoc: "Documentos",
  studioStepCommand: "Ejecutar comando",
  studioStepInstall: "Instalar dependencias",
  studioStepEcho: "Mostrar log",
  studioStepEnv: "Definir variable de entorno",
  studioStepWorkDir: "Cambiar directorio",
  studioStepPath: "Añadir PATH",
  studioStepTest: "Ejecutar pruebas + informe",
  studioStepHealthcheck: "Comprobación de salud",
  studioStepArtifact: "Subir artefacto",
  studioStepDownload: "Descargar archivo",
  studioStepExtract: "Extraer archivo",
  studioStepCondition: "Guarda condicional",
  studioStepRetry: "Envoltura de reintento",
  studioStepTimeout: "Límite de tiempo de espera",
  studioStepSleep: "Espera / retardo",
  studioStepNote: "Nota",
  studioDefaultCategory: "Personalizado",

  // ─── stageSettings · when events ───────────────────────────────────────
  whenManual: "Manual",
  whenSchedule: "Programado",

  // ─── stageSettings · post conditions ───────────────────────────────────
  postAlways: "Siempre",
  postOnSuccess: "En éxito",
  postOnFailure: "En fallo",

  // ─── stageSettings · matrix errors ─────────────────────────────────────
  matrixAxisInvalid:
    "El nombre de eje «{name}» no es válido (debe ser un identificador: empieza con letra/guion bajo)",
  matrixAxisNeedsValue: "El eje «{name}» necesita al menos un valor",
  matrixTooManyAxes: "La dimensión de matriz {count} supera el límite {max}",
  matrixTooManyCells:
    "La matriz se expande a {cells} celdas, superando el límite {max}",

  // ─── JobDrawer.vue ─────────────────────────────────────────────────────
  drawerAria: "Configuración de la tarea",
  unnamedJob: "(tarea sin nombre)",
  thisPipeline: "este pipeline",
  closeDrawer: "Cerrar panel",
  basicInfo: "Información básica",
  jobNameLabel: "Nombre de la tarea",
  jobNamePlaceholder: "p. ej.: Build aislado",
  jobTypeLabel: "Tipo de tarea",
  changeType: "Cambiar el tipo de tarea",
  change: "Cambiar",
  summaryLabel: "Descripción del resumen",
  summaryPlaceholder: "Subtítulo de la tarjeta (opcional)",
  instanceParams: "Parámetros de instancia",
  instanceParamsHint:
    "Este nodo personalizado expone los siguientes parámetros; ajústalos según necesites, el resto de la plantilla de comandos está fijado por el autor del nodo.",
  advancedRawToggle: "Avanzado · Ver/editar parámetros sin procesar",
  configSuffix: " configuración",
  configView: "Vista de configuración",
  visualSteps: "Pasos visuales",
  rawParams: "Parámetros sin procesar",
  execAdvancedToggle:
    "Avanzado · Tiempo de espera / reintentos / recursos / caché",
  toggleEnable: "Activar",
  stepBuilderHint:
    "Los pasos se compilan en comandos multilínea que se ejecutan dentro de un contenedor aislado; cambiar directorio / definir variable de entorno afecta a los comandos posteriores. Puedes cambiar a «Parámetros sin procesar» en cualquier momento para verlos.",
  advancedRawParams: "Avanzado · Parámetros sin procesar",
  kvEmptyWithSpec:
    "Los parámetros de este tipo están en el formulario de arriba; aquí puedes añadir clave-valor personalizados",
  kvEmptyNoSpec: "Añade parámetros clave-valor personalizados para este tipo",
  kvKeyPlaceholder: "key",
  kvValuePlaceholder: "value",
  kvKeyAria: "Clave de configuración {n}",
  kvValueAria: "Valor de configuración {n}",
  kvDelAria: "Eliminar elemento de configuración {label}",
  addParam: "Añadir parámetro",
  credUnselected: "— Sin seleccionar —",
  channelUnselected: "— Seleccionar canal —",
  channelDisabled: "(desactivado)",
  saveAsCustomNode: "Guardar como nodo personalizado",
  savedToLibrary: "✓ Guardado en la biblioteca de reutilización",
  savePanelHint:
    "Guarda una instantánea del tipo y los parámetros de este nodo en la biblioteca de reutilización, para luego elegirlo en cualquier pipeline.",
  saveNamePlaceholder: "Nombre del nodo (único en la biblioteca)",
  saveNameAria: "Nombre del nodo personalizado",
  saveDescPlaceholder: "Descripción (opcional)",
  saveDescAria: "Descripción del nodo personalizado",
  cancel: "Cancelar",
  saving: "Guardando…",
  save: "Guardar",
  saveErrEmptyName: "Introduce un nombre de nodo",
  saveErrFailed:
    "Error al guardar, inténtalo de nuevo (puede ser un nombre duplicado)",
  channelTypeEmail: "Correo",
  channelTypeFeishu: "Feishu",
  channelTypeWecom: "WeCom",
  channelTypeDingtalk: "DingTalk",

  // ─── StepBuilder.vue ───────────────────────────────────────────────────
  sbVisualSteps: "Pasos visuales",
  sbStepCount: "Número de pasos",
  sbDragSort: "Arrastra para reordenar",
  sbMoveUp: "Subir el paso {n}",
  sbMoveDown: "Bajar el paso {n}",
  sbDelStep: "Eliminar el paso {n}",
  sbCommandAria: "Comando {n}",
  sbEnvKeyAria: "Nombre de variable de entorno {n}",
  sbEnvValueAria: "Valor de variable de entorno {n}",
  sbDirAria: "Directorio {n}",
  sbCondAria: "Condición {n}",
  sbCondHintPre: "condición de shell; si no se cumple, ",
  sbCondHintStrong: "omite todos los pasos posteriores",
  sbCondHintPost: " (termina con éxito). Por ejemplo",
  sbArtifactAria: "Ruta de artefacto {n}",
  sbEmpty: "Aún no hay pasos, haz clic en «Añadir paso» abajo para empezar",
  sbAddStep: "Añadir paso",
  sbAddCommandDesc: "comando de shell (multilínea)",
  sbAddEnvDesc: "inyectar KEY=VALUE en el entorno",
  sbAddWorkDirDesc:
    "cambiar el directorio de trabajo para comandos posteriores",
  sbAddConditionDesc: "omite pasos posteriores si no se cumple",
  sbAddArtifactDesc: "glob para archivar artefactos",
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
