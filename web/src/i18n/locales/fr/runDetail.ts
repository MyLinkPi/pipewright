export default {
  title: "Détails de l'exécution",
  runList: "Exécutions",
  backToRunsAria: "Retour à la liste des exécutions",
  loadingAria: "Chargement des détails de l'exécution",
  retry: "Réessayer",
  runNotFound: "L'exécution {id} n'existe pas",
  loadFailed: "Échec du chargement ({status})",
  loadRequestFailed:
    "Échec du chargement des détails de l'exécution, réessayez plus tard",

  statusAria: "Statut de l'exécution : {status}",
  runStatusAria: "Exécution {status}",

  cancelRun: "Annuler l'exécution",
  cancelling: "Annulation…",
  cancelFailed: "Échec de l'annulation ({status})",
  cancelRequestFailed: "La demande d'annulation a échoué, réessayez plus tard",

  approvalRegionAria: "En attente de validation manuelle",
  approvalTitle: "L'étape « {stage} » est en attente de validation manuelle",
  approvalSub:
    "Approuver poursuit l'exécution ; refuser fait échouer cette étape et arrête l'exécution.",
  reject: "Rejeter",
  approve: "Approuver",
  approving: "Traitement…",
  actionFailed: "Échec de l'action ({status})",
  approvalRequestFailed:
    "La demande de validation a échoué, réessayez plus tard",

  metaProject: "Projet",
  metaBranch: "Branche",
  metaCommit: "Commit",
  metaTrigger: "Déclencheur",
  metaStarted: "Début",
  metaDuration: "Durée",
  triggerManual: "Manuel",
  commitUnresolved: "Non récupéré",

  // Badge de source de configuration (GitOps · Slice 2)
  metaSpecSource: "Source de configuration",
  specSourceRepo: "Dépôt {ref} · {file}",
  specSourceStored: "Configuration web",
  specSourceFallbackHint:
    "Fichier du dépôt manquant ou invalide ; repli sur la configuration web",

  pipelineProgress: "Progression du pipeline",
  pipelineComplete: "Pipeline terminé",
  pipelineFailed: "Pipeline en échec",
  stepRecord: "Journal des étapes",

  liveLogAria: "Terminal de journal en direct",
  historyLogAria: "Journal d'exécution historique",
  failedLogAria: "Journal de l'exécution en échec",
  diffAria: "Comparaison des différences de code entre succès et échec",

  finishedTime: "Terminé le",
  totalDuration: "Durée totale",
  endTime: "Heure de fin",

  batchPausedPrefix: "Déploiement par lots en pause : premier lot publié, les ",
  batchPausedSuffix: " hôte(s) restant(s) en attente de confirmation",
  processing: "Traitement…",
  continueRest: "Continuer avec le reste",
  abortKeepOld: "Abandonner (conserver l'ancienne version)",
  continueFailed: "Échec de la poursuite ({status})",
  continueRequestFailed:
    "La demande de poursuite a échoué, réessayez plus tard",
  abortFailed: "Échec de l'abandon ({status})",
  abortRequestFailed: "La demande d'abandon a échoué, réessayez plus tard",
  noArtifactContinue: "Aucun artefact disponible, impossible de continuer",

  deployToServers: "Déployer sur les serveurs cibles",
  deployAgain: "Déployer à nouveau",
  deployConfigAria: "Configuration du déploiement",
  deployCloseAria: "Réduire le panneau de déploiement",
  deployArtifact: "Artefact à déployer",
  targetSelector: "Sélecteur de cibles",
  selectorPlaceholder: "ex. web,env=prod ou server:<id>",
  selectorHint:
    "Termes d'étiquettes séparés par des virgules, tous doivent correspondre ; server:<id> fixe une machine ; vide = ignorer le déploiement. Prioritaire sur le champ serveur cible",
  selectorNoMatch: "Aucun serveur ne correspond à ce sélecteur",
  selectorMatchCount: "{n} correspondances",
  noServers:
    "Aucun serveur enregistré pour l'instant. Enregistrez-en un d'abord sur la page « Serveurs ».",

  healthCheck: "Contrôle de santé",
  hcNone:
    "Aucun contrôle (le succès de la commande vaut un déploiement réussi)",
  hcHttp: "Sonde HTTP (curl)",
  hcCommand: "Sonde par commande",
  hcUrlAria: "URL du contrôle de santé",
  hcCommandPlaceholder: "ex. : systemctl is-active shop",
  hcCommandAria: "Commande du contrôle de santé",
  hcRetries: "Nombre de tentatives",
  hcInterval: "Intervalle (s)",
  hcTimeout: "Délai d'attente (s)",
  hcUrlRequired: "Veuillez saisir une URL de sonde.",
  hcCommandRequired: "Veuillez saisir une commande de sonde.",

  releaseStrategy: "Stratégie de publication",
  strategyInstanceRollingLabel: "Rolling d’instances (défaut)",
  strategyInstanceRollingDesc: "bascule sans interruption par instance",
  strategyRollingLabel: "Progressive",
  strategyRollingDesc:
    "Tous les hôtes en parallèle, chacun réussit ou échoue indépendamment",
  strategyCanaryLabel: "Canary",
  strategyCanaryDesc:
    "Publier un petit lot d'abord, déployer le reste une fois validé",
  strategyBlueGreenLabel: "Bleu-vert",
  strategyBlueGreenDesc: "Bascule unifiée, retour arrière total en cas d'échec",
  strategyInteractiveLabel: "Lots interactifs",
  strategyInteractiveDesc:
    "Publier le premier lot, puis suspendre pour confirmation manuelle",
  canaryCount: "Hôtes canary",
  canaryHint:
    "Publier d'abord ce nombre d'hôtes sous contrôle de santé, puis déployer le reste une fois validés",
  blueGreenHint:
    "Tous les hôtes préparent d'abord le répertoire de publication, puis basculent atomiquement ensemble ; si un hôte échoue à basculer, l'ensemble du parc revient à la publication précédente (dist / jar).",

  advancedToggle: "Avancé : options de publication sans interruption",
  releaseBase: "Répertoire racine de publication",
  releaseBasePlaceholder:
    "Laisser vide → le backend le déduit du chemin de déploiement (<base>/releases/<runId> + <base>/current)",
  keepReleases: "Anciennes publications à conserver",
  advancedHint:
    "dist / jar sont déployés dans un répertoire de publication versionné avec une bascule atomique du lien symbolique current ; en cas d'échec du contrôle de santé, retour arrière automatique vers la publication précédente.",

  startDeploy: "Lancer le déploiement",
  deploying: "Déploiement…",
  deploySkippedNoMatch:
    "Le sélecteur n'a correspondu à aucun serveur — déploiement ignoré (état de l'exécution inchangé)",
  deploySkippedEmpty:
    "Aucun sélecteur de cible défini — déploiement ignoré (état de l'exécution inchangé)",
  deployFailed: "Échec du déploiement ({status})",
  deployRequestFailed:
    "La demande de déploiement a échoué, réessayez plus tard",

  noArtifactRetry: "Aucun artefact disponible, impossible de réessayer",
  retryFailed: "Échec de la nouvelle tentative ({status})",
  retryRequestFailed:
    "La demande de nouvelle tentative a échoué, réessayez plus tard",

  // Reprise par nœud(exécution dérivée d'un échec)
  resumeRegionAria: "Reprendre l'exécution par nœud",
  resumeTitle: "Reprendre l'exécution par nœud",
  resumeDesc: "Cliquez sur Réessayer / Ignorer pour créer immédiatement une exécution dérivée : le nœud cliqué suit l'action choisie, les autres nœuds en échec réessaient par défaut ; les nœuds réussis sont hérités et les nœuds de déploiement ne réessaient que les machines en échec.",
  resumeRetry: "Réessayer",
  resumeSkip: "Ignorer",
  resumeSkipHint: "Ignore ce nœud ; l'aval s'exécute normalement",
  resumeLaunching: "Création…",
  resumeSpecChanged: "La configuration du pipeline a changé — reprise par nœud impossible",
  resumeNotResumable: "Cette exécution ne peut pas être reprise par nœud",
  resumeRunNotFound: "Exécution introuvable",
  resumeQueueFull: "File d'attente de planification pleine, réessayez plus tard",
  resumeRequestFailed: "Échec de la reprise ({status})",
  resumedFrom: "Reprise depuis #{id}",

  partialInfo:
    "Certaines cibles ont échoué et les hôtes en échec ont effectué un retour arrière indépendant ; les autres continuent de s'exécuter sans être affectés.",
  multiTargetAria: "Statut des cibles multi-hôtes",
  multiTargetFanout: "Distribution des cibles multi-hôtes",
  noMultiResult: "Aucun résultat de déploiement multi-hôtes pour le moment",
  rollingBatches: 'Rolling batches',
  firstBatchSize: 'First batch',
  batchSizeEach: 'Batch size',
  rollingBatchesHint: 'First batch verifies small (default 1 host); then N hosts per batch, 0 = all remaining at once; any batch failure stops the rollout.',
  selectorMatchMode: 'Label matching',
  selectorModeAll: 'Match all terms (AND)',
  selectorModeAny: 'Match any term (OR)',
};
