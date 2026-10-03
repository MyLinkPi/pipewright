export default {
  title: "Ausführungsdetails",
  runList: "Ausführungen",
  backToRunsAria: "Zurück zur Ausführungsliste",
  loadingAria: "Ausführungsdetails werden geladen",
  retry: "Wiederholen",
  runNotFound: "Ausführung {id} existiert nicht",
  loadFailed: "Laden fehlgeschlagen ({status})",
  loadRequestFailed:
    "Ausführungsdetails konnten nicht geladen werden, bitte später erneut versuchen",

  statusAria: "Ausführungsstatus: {status}",
  runStatusAria: "Ausführung {status}",

  cancelRun: "Ausführung abbrechen",
  cancelling: "Wird abgebrochen…",
  cancelFailed: "Abbruch fehlgeschlagen ({status})",
  cancelRequestFailed:
    "Abbruchanfrage fehlgeschlagen, bitte später erneut versuchen",

  approvalRegionAria: "Warten auf manuelle Genehmigung",
  approvalTitle: "Phase „{stage}“ wartet auf manuelle Genehmigung",
  approvalSub:
    "Genehmigen setzt die Ausführung fort; Ablehnen lässt diese Phase fehlschlagen und beendet die Ausführung.",
  reject: "Ablehnen",
  approve: "Genehmigen",
  approving: "Wird verarbeitet…",
  actionFailed: "Aktion fehlgeschlagen ({status})",
  approvalRequestFailed:
    "Genehmigungsanfrage fehlgeschlagen, bitte später erneut versuchen",

  metaProject: "Projekt",
  metaBranch: "Branch",
  metaCommit: "Commit",
  metaTrigger: "Auslöser",
  metaStarted: "Start",
  metaDuration: "Dauer",
  triggerManual: "Manuell",
  commitUnresolved: "Nicht ermittelt",

  // Badge für Konfigurationsquelle (GitOps · Slice 2)
  metaSpecSource: "Konfigurationsquelle",
  specSourceRepo: "Repository {ref} · {file}",
  specSourceStored: "Web-Konfiguration",
  specSourceFallbackHint:
    "Repository-Datei fehlt oder ist ungültig; auf Web-Konfiguration zurückgegriffen",

  pipelineProgress: "Pipeline-Fortschritt",
  pipelineComplete: "Pipeline abgeschlossen",
  pipelineFailed: "Pipeline fehlgeschlagen",
  stepRecord: "Schrittprotokoll",

  liveLogAria: "Echtzeit-Log-Terminal",
  historyLogAria: "Verlaufsprotokoll der Ausführung",
  failedLogAria: "Protokoll der fehlgeschlagenen Ausführung",
  diffAria: "Code-Vergleich zwischen Erfolg und Fehler",

  finishedTime: "Abgeschlossen am",
  totalDuration: "Gesamtdauer",
  endTime: "Endzeit",

  batchPausedPrefix:
    "Batch-Bereitstellung pausiert: erster Batch veröffentlicht, die verbleibenden ",
  batchPausedSuffix: " Host(s) warten auf Bestätigung",
  processing: "Wird verarbeitet…",
  continueRest: "Mit dem Rest fortfahren",
  abortKeepOld: "Abbrechen (alte Version behalten)",
  continueFailed: "Fortsetzen fehlgeschlagen ({status})",
  continueRequestFailed:
    "Anfrage zum Fortsetzen fehlgeschlagen, bitte später erneut versuchen",
  abortFailed: "Abbruch fehlgeschlagen ({status})",
  abortRequestFailed:
    "Abbruchanfrage fehlgeschlagen, bitte später erneut versuchen",
  noArtifactContinue: "Kein Artefakt verfügbar, Fortsetzen nicht möglich",

  deployToServers: "Auf Zielserver bereitstellen",
  deployAgain: "Erneut bereitstellen",
  deployConfigAria: "Bereitstellungskonfiguration",
  deployCloseAria: "Bereitstellungspanel einklappen",
  deployArtifact: "Bereitzustellendes Artefakt",
  targetSelector: "Ziel-Selektor",
  selectorPlaceholder: "z. B. web,env=prod oder server:<id>",
  selectorHint:
    "Komma-getrennte Label-Begriffe, alle müssen treffen; server:<id> fixiert eine Maschine; leer = Deployment überspringen. Hat Vorrang vor dem Zielserver-Feld",
  selectorNoMatch: "Keine Server treffen diesen Selektor",
  selectorMatchCount: "{n} getroffen",
  noServers:
    "Noch keine registrierten Server. Bitte registriere zuerst einen auf der Seite „Server“.",

  healthCheck: "Health-Check",
  hcNone: "Keine Prüfung (Befehlserfolg gilt als erfolgreiche Bereitstellung)",
  hcHttp: "HTTP-Probe (curl)",
  hcCommand: "Befehls-Probe",
  hcUrlAria: "Health-Check-URL",
  hcCommandPlaceholder: "z. B. systemctl is-active shop",
  hcCommandAria: "Health-Check-Befehl",
  hcRetries: "Wiederholungen",
  hcInterval: "Intervall (s)",
  hcTimeout: "Zeitlimit (s)",
  hcUrlRequired: "Bitte eine Probe-URL eingeben.",
  hcCommandRequired: "Bitte einen Probe-Befehl eingeben.",

  releaseStrategy: "Release-Strategie",
  strategyInstanceRollingLabel: "Instance-Rolling (Standard)",
  strategyInstanceRollingDesc: "instanzweiser Wechsel ohne Ausfallzeit",
  strategyRollingLabel: "Rollend",
  strategyRollingDesc: "Alle Hosts parallel, jeder mit eigenem Ergebnis",
  strategyCanaryLabel: "Canary",
  strategyCanaryDesc:
    "Zuerst eine kleine Charge ausrollen, den Rest nach bestandenem Test",
  strategyBlueGreenLabel: "Blau-Grün",
  strategyBlueGreenDesc:
    "Einheitliche Umschaltung, bei Fehler vollständiger Rollback",
  strategyInteractiveLabel: "Interaktive Chargen",
  strategyInteractiveDesc:
    "Erste Charge ausrollen, dann für manuelle Bestätigung pausieren",
  canaryCount: "Canary-Hosts",
  canaryHint:
    "Zuerst so viele Hosts mit Health-Gating ausrollen, dann nach bestandenem Test den Rest",
  blueGreenHint:
    "Alle Hosts bereiten zuerst das Release-Verzeichnis vor und schalten dann gemeinsam atomar um; schlägt die Umschaltung bei einem Host fehl, rollt die gesamte Flotte auf das vorherige Release zurück (dist / jar).",

  advancedToggle: "Erweitert: Optionen für Zero-Downtime-Release",
  releaseBase: "Release-Stammverzeichnis",
  releaseBasePlaceholder:
    "Leer lassen → das Backend leitet es aus dem Bereitstellungspfad ab (<base>/releases/<runId> + <base>/current)",
  keepReleases: "Aufzubewahrende alte Releases",
  advancedHint:
    "dist / jar werden in ein versioniertes Release-Verzeichnis bereitgestellt und der current-Symlink atomar umgeschaltet; bei fehlgeschlagenem Health-Check erfolgt ein automatischer Rollback auf das vorherige Release.",

  startDeploy: "Bereitstellung starten",
  deploying: "Wird bereitgestellt…",
  deploySkippedNoMatch:
    "Selektor traf keine Server — Deployment übersprungen (Run-Status unverändert)",
  deploySkippedEmpty:
    "Kein Ziel-Selektor gesetzt — Deployment übersprungen (Run-Status unverändert)",
  deployFailed: "Bereitstellung fehlgeschlagen ({status})",
  deployRequestFailed:
    "Bereitstellungsanfrage fehlgeschlagen, bitte später erneut versuchen",

  noArtifactRetry: "Kein Artefakt verfügbar, Wiederholung nicht möglich",
  retryFailed: "Wiederholung fehlgeschlagen ({status})",
  retryRequestFailed:
    "Wiederholungsanfrage fehlgeschlagen, bitte später erneut versuchen",

  partialInfo:
    "Einige Ziele sind fehlgeschlagen und die fehlgeschlagenen Hosts wurden unabhängig zurückgerollt; die übrigen laufen unbeeinträchtigt weiter.",
  multiTargetAria: "Status der Multi-Host-Ziele",
  multiTargetFanout: "Multi-Host-Ziel-Fan-out",
  noMultiResult: "Noch keine Multi-Host-Bereitstellungsergebnisse",
  rollingBatches: 'Rolling batches',
  firstBatchSize: 'First batch',
  batchSizeEach: 'Batch size',
  rollingBatchesHint: 'First batch verifies small (default 1 host); then N hosts per batch, 0 = all remaining at once; any batch failure stops the rollout.',
  selectorMatchMode: 'Label matching',
  selectorModeAll: 'Match all terms (AND)',
  selectorModeAny: 'Match any term (OR)',
};
