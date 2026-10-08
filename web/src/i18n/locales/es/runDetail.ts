export default {
  title: "Detalles de la ejecución",
  runList: "Ejecuciones",
  backToRunsAria: "Volver a la lista de ejecuciones",
  loadingAria: "Cargando detalles de la ejecución",
  retry: "Reintentar",
  runNotFound: "La ejecución {id} no existe",
  loadFailed: "Error al cargar ({status})",
  loadRequestFailed:
    "No se pudieron cargar los detalles de la ejecución, inténtalo de nuevo más tarde",

  statusAria: "Estado de la ejecución: {status}",
  runStatusAria: "Ejecución {status}",

  cancelRun: "Cancelar ejecución",
  cancelling: "Cancelando…",
  cancelFailed: "Error al cancelar ({status})",
  cancelRequestFailed:
    "La solicitud de cancelación falló, inténtalo de nuevo más tarde",

  approvalRegionAria: "Esperando aprobación manual",
  approvalTitle: "La etapa «{stage}» está esperando aprobación manual",
  approvalSub:
    "Aprobar continúa la ejecución; rechazar hace fallar esta etapa y detiene la ejecución.",
  reject: "Rechazar",
  approve: "Aprobar",
  approving: "Procesando…",
  actionFailed: "Error en la acción ({status})",
  approvalRequestFailed:
    "La solicitud de aprobación falló, inténtalo de nuevo más tarde",

  metaProject: "Proyecto",
  metaBranch: "Rama",
  metaCommit: "Commit",
  metaTrigger: "Disparador",
  metaStarted: "Inicio",
  metaDuration: "Duración",
  triggerManual: "Manual",
  commitUnresolved: "No obtenido",

  // Insignia de origen de configuración (GitOps · Slice 2)
  metaSpecSource: "Origen de configuración",
  specSourceRepo: "Repositorio {ref} · {file}",
  specSourceStored: "Configuración web",
  specSourceFallbackHint:
    "El archivo del repositorio falta o no es válido; se recurrió a la configuración web",

  pipelineProgress: "Progreso del pipeline",
  pipelineComplete: "Pipeline completado",
  pipelineFailed: "Pipeline fallido",
  stepRecord: "Registro de pasos",

  liveLogAria: "Terminal de registro en vivo",
  historyLogAria: "Registro histórico de la ejecución",
  failedLogAria: "Registro de la ejecución fallida",
  diffAria: "Comparación de diferencias de código entre éxito y fallo",

  finishedTime: "Finalizado el",
  totalDuration: "Duración total",
  endTime: "Hora de fin",

  batchPausedPrefix:
    "Despliegue por lotes en pausa: primer lote publicado, los ",
  batchPausedSuffix: " host(s) restante(s) a la espera de confirmación",
  processing: "Procesando…",
  continueRest: "Continuar con el resto",
  abortKeepOld: "Abortar (mantener la versión anterior)",
  continueFailed: "Error al continuar ({status})",
  continueRequestFailed:
    "La solicitud para continuar falló, inténtalo de nuevo más tarde",
  abortFailed: "Error al abortar ({status})",
  abortRequestFailed:
    "La solicitud de aborto falló, inténtalo de nuevo más tarde",
  noArtifactContinue: "No hay artefacto disponible, no se puede continuar",

  deployToServers: "Desplegar en servidores de destino",
  deployAgain: "Desplegar de nuevo",
  deployConfigAria: "Configuración del despliegue",
  deployCloseAria: "Contraer el panel de despliegue",
  deployArtifact: "Artefacto a desplegar",
  targetSelector: "Selector de destino",
  selectorPlaceholder: "p. ej. web,env=prod o server:<id>",
  selectorHint:
    "Términos de etiqueta separados por comas, todos deben coincidir; server:<id> fija una máquina; vacío = omitir despliegue. Tiene prioridad sobre el campo de servidor de destino",
  selectorNoMatch: "Ningún servidor coincide con este selector",
  selectorMatchCount: "{n} coinciden",
  noServers:
    "Aún no hay servidores registrados. Regístralos primero en la página «Servidores».",

  healthCheck: "Comprobación de estado",
  hcNone:
    "Sin comprobación (el éxito del comando cuenta como despliegue exitoso)",
  hcHttp: "Sonda HTTP (curl)",
  hcCommand: "Sonda por comando",
  hcUrlAria: "URL de comprobación de estado",
  hcCommandPlaceholder: "p. ej. systemctl is-active shop",
  hcCommandAria: "Comando de comprobación de estado",
  hcRetries: "Reintentos",
  hcInterval: "Intervalo (s)",
  hcTimeout: "Tiempo de espera (s)",
  hcUrlRequired: "Introduce una URL de sonda.",
  hcCommandRequired: "Introduce un comando de sonda.",

  releaseStrategy: "Estrategia de publicación",
  strategyInstanceRollingLabel: "Rolling de instancias (por defecto)",
  strategyInstanceRollingDesc: "cambio sin paradas por instancia",
  strategyRollingLabel: "Progresiva",
  strategyRollingDesc:
    "Todos los hosts en paralelo, cada uno con su propio resultado",
  strategyCanaryLabel: "Canary",
  strategyCanaryDesc:
    "Publica un lote pequeño primero y despliega el resto cuando pase",
  strategyBlueGreenLabel: "Azul-verde",
  strategyBlueGreenDesc: "Cambio unificado, retroceso total si falla",
  strategyInteractiveLabel: "Lotes interactivos",
  strategyInteractiveDesc:
    "Publica el primer lote y luego pausa para confirmación manual",
  canaryCount: "Hosts canary",
  canaryHint:
    "Publica primero esta cantidad de hosts con control de estado y despliega el resto cuando pasen",
  blueGreenHint:
    "Todos los hosts preparan primero el directorio de publicación y luego cambian de forma atómica a la vez; si algún host falla el cambio, toda la flota retrocede a la publicación anterior (dist / jar).",

  advancedToggle: "Avanzado: opciones de publicación sin tiempo de inactividad",
  releaseBase: "Directorio raíz de publicación",
  releaseBasePlaceholder:
    "Déjalo vacío → el backend lo deriva de la ruta de despliegue (<base>/releases/<runId> + <base>/current)",
  keepReleases: "Publicaciones antiguas a conservar",
  advancedHint:
    "dist / jar se despliegan en un directorio de publicación versionado y cambian el enlace simbólico current de forma atómica; si la comprobación de estado falla, retrocede automáticamente a la publicación anterior.",

  startDeploy: "Iniciar despliegue",
  deploying: "Desplegando…",
  deploySkippedNoMatch:
    "El selector no coincidió con ningún servidor — despliegue omitido (estado de la ejecución sin cambios)",
  deploySkippedEmpty:
    "Sin selector de destino — despliegue omitido (estado de la ejecución sin cambios)",
  deployFailed: "Error al desplegar ({status})",
  deployRequestFailed:
    "La solicitud de despliegue falló, inténtalo de nuevo más tarde",

  noArtifactRetry: "No hay artefacto disponible, no se puede reintentar",
  retryFailed: "Error al reintentar ({status})",
  retryRequestFailed:
    "La solicitud de reintento falló, inténtalo de nuevo más tarde",

  // Reanudar por nodo(ejecución derivada de una fallida)
  resumeRegionAria: "Reanudar ejecución por nodo",
  resumeTitle: "Reanudar ejecución por nodo",
  resumeDesc: "Pulsa Reintentar / Omitir para crear al instante una ejecución derivada: el nodo pulsado sigue la acción elegida, los demás nodos fallidos reintentan por defecto; los nodos exitosos se heredan y los de despliegue solo reintentan las máquinas fallidas.",
  resumeRetry: "Reintentar",
  resumeSkip: "Omitir",
  resumeSkipHint: "Omite este nodo; el downstream se ejecuta con normalidad",
  resumeLaunching: "Creando…",
  resumeSpecChanged: "La configuración del pipeline cambió; no se puede reanudar por nodo",
  resumeNotResumable: "Esta ejecución no se puede reanudar por nodo",
  resumeRunNotFound: "Ejecución no encontrada",
  resumeQueueFull: "La cola de programación está llena, inténtalo de nuevo más tarde",
  resumeRequestFailed: "Error al reanudar ({status})",
  resumedFrom: "Reanudada desde #{id}",

  partialInfo:
    "Algunos destinos fallaron y los hosts fallidos han retrocedido de forma independiente; el resto siguen ejecutándose sin verse afectados.",
  multiTargetAria: "Estado de destinos multi-host",
  multiTargetFanout: "Distribución de destinos multi-host",
  noMultiResult: "Aún no hay resultados de despliegue multi-host",
  rollingBatches: 'Rolling batches',
  firstBatchSize: 'First batch',
  batchSizeEach: 'Batch size',
  rollingBatchesHint: 'First batch verifies small (default 1 host); then N hosts per batch, 0 = all remaining at once; any batch failure stops the rollout.',
  selectorMatchMode: 'Label matching',
  selectorModeAll: 'Match all terms (AND)',
  selectorModeAny: 'Match any term (OR)',
};
