export default {
  protocolHelp:
    'Choisissez le format API, pas le fournisseur. DeepSeek, OpenRouter et les passerelles privées fonctionnent avec un protocole compatible. Renseignez leur URL de base, clé API et identifiant du modèle.',
  compatibleProtocol: 'Protocole compatible',
  selectProtocol: 'Choisir le protocole API',
  quickSetup: 'Configuration rapide',
  customGateway: 'Passerelle personnalisée',
  modelId: 'ID du modèle',
  availableModels: 'Modèles disponibles',
  fetchModels: 'Obtenir les modèles',
  modelsUnavailable: 'Liste indisponible ; saisir un ID manuellement.',
  title: 'Fournisseur AI',
  subtitle:
    'Pipewright n’entraîne aucun modèle propre — connectez votre propre LLM pour le diagnostic des échecs et la génération de configuration. Les clés ne résident que dans le coffre chiffré de cette instance et n’en sortent jamais.',
  statusConfigured: 'Configuré',
  statusUnconfigured: 'Non configuré',
  retry: 'Réessayer',

  providerClaudeTag: 'Recommandé pour le diagnostic',

  guidanceAria: 'Guide de configuration AI',
  guidanceTitle: 'Configurez un LLM pour activer le diagnostic AI',
  guidanceBody:
    'Une fois Claude, OpenAI ou tout endpoint compatible OpenAI (ex. Ollama /v1) connecté, Pipewright génère automatiquement des hypothèses de cause racine et des suggestions de correction lorsqu’un pipeline échoue, sans avoir à fouiller les journaux manuellement.',

  selectProvider: 'Sélectionnez un fournisseur',
  providerRadioAria: 'Sélection du fournisseur AI',
  selectProviderAria: 'Sélectionner {name}',
  providerConfig: 'Configuration de {name}',
  lastSaved: 'Dernier enregistrement {time}',

  apiKeyHint:
    'Seule une valeur masquée s’affiche après l’enregistrement ; laissez vide pour conserver la clé existante',
  apiKeyReplacing: 'Remplacement…',
  apiKeyConfigured: 'Configurée •••• (vide pour conserver)',
  apiKeyPaste: 'Collez la API Key…',
  apiKeyMaskedAria: 'Masque configuré : {masked}',


  baseUrlLabel: 'Base URL',
  baseUrlHint: 'Par défaut : {url} (concaténée telle quelle — conserver les préfixes de version tels que /v1)',

  modelLabel: 'Modèle',
  modelHint:
    'Modèle principal utilisé pour le diagnostic, par ex. claude-opus-4-7 / gpt-4o / llama3',

  testConnection: 'Tester la connexion',
  testOk: 'Inférence correcte · latence {ms}ms',
  testFail: 'Échec de la connexion',

  budgetLabel: 'Limite mensuelle de Token',
  budgetHint:
    'Met en pause le diagnostic AI une fois dépassée (vide = illimité ; déclarée ce cycle, appliquée au prochain Epic)',
  budgetPlaceholder: 'par ex. 500000, vide = illimité',

  ctxTitle: 'Gestion du contexte',
  ctxWindowHint:
    "Budget de contexte de l'agent ops : près de la limite de fenêtre, l'historique ancien est compacté automatiquement en résumé ; en cas de débordement, compactage puis nouvelle tentative. Valeurs en tokens, vide = valeur par défaut.",
  ctxWindowLabel: 'Fenêtre de contexte',
  ctxWindowPlaceholder: 'Par défaut 200000',
  ctxMaxOutputLabel: 'Sortie max par appel',
  ctxMaxOutputPlaceholder: 'Par défaut 8192',
  ctxKeepLabel: 'À conserver lors du compactage',
  ctxKeepPlaceholder: 'Par défaut 15000',
  ctxAutoCompact: "Compacter l'historiel près de la limite",
  ctxAdvanced: 'Avancé',
  ctxBufferLabel: 'Réserve de déclenchement',
  ctxBufferPlaceholder: 'Auto (10 % de la fenêtre)',
  ctxHistoryMessagesLabel: 'Limite de messages',
  ctxHistoryMessagesPlaceholder: 'Par défaut 100',
  ctxHistoryBytesLabel: 'Limite de taille (Ko)',
  ctxHistoryBytesPlaceholder: 'Par défaut 256',
  ctxNote:
    "Le compactage n'affecte que le contexte envoyé au modèle ; la vue de session conserve tout l'historique. Les résumés sont générés par le modèle configuré.",
  errContextInvalid: 'Les valeurs de contexte doivent être des entiers non négatifs ou vides',

  enableAi: 'Activer les fonctions AI',
  enableAiDesc:
    'Désactivé, le diagnostic AI est ignoré silencieusement et les pipelines CI/CD principaux ne sont pas affectés',

  dirtyNote: 'Vous avez des modifications non enregistrées',
  cleanNote: 'Aucune modification',
  discard: 'Abandonner',
  saveChanges: 'Enregistrer les modifications',

  toastSaveSuccess: 'Configuration AI enregistrée',
  toastSaveFailed: 'Échec de l’enregistrement',

  errServerUnreachable:
    'Impossible de joindre le serveur. Vérifiez que le backend est en cours d’exécution et réessayez.',
  errServerUnreachableShort: 'Impossible de joindre le serveur',
  errVaultUnconfigured:
    'Le coffre n’a pas de master key configurée. Définissez la variable d’environnement PIPEWRIGHT_MASTER_KEY.',
  errLoadFailed: 'Échec du chargement ({status})',
  errLoadGeneric:
    'Échec du chargement de la configuration AI. Veuillez réessayer plus tard.',
  errBudgetInvalid:
    'La limite mensuelle de tokens doit être un entier positif ou laissée vide',
  errProviderInvalid: 'Veuillez sélectionner un fournisseur valide',
  errBaseUrlRequired: 'Veuillez saisir la base URL',
  errApiKeyRequired:
    'La API Key ne peut pas être vide (toute valeur non vide convient aux endpoints locaux)',
  errRequestFailed: 'Échec de la requête ({status})',
  errUnknown: 'Erreur inconnue',
  webTitle: 'Recherche et lecture web',
  webEnable: 'Activer recherche/lecture web',
  webEnableDesc: 'Ajoute web_search / web_fetch à l’agent ops (facturé à l’appel, désactivé par défaut)',
  webFollowLLM: 'Suivre la config LLM (défaut)',
  webProviderLabel: 'Fournisseur',
  webProviderHint: 'Réutilise par défaut l’URL et la clé du LLM (disponible uniquement si le LLM pointe vers DeepSeek officiel) ; choisissez un fournisseur pour configurer séparément',
  webBaseUrlLabel: 'URL de base',
  webBaseUrlHint: 'Laissez vide pour la valeur officielle : {url}',
  webApiKeyHint: 'Vide réutilise la clé du LLM si l’URL correspond ; écriture seule, ensuite masquée',
  webTest: 'Tester la recherche',
  webAvailable: 'Outils web prêts (destination et clé résolues)',
  webUnavailable: 'La configuration actuelle est inutilisable',
  webNote: 'web_search interroge le web public ; web_fetch lit une page en Markdown. Les appels de l’agent sont facturés par fournisseur (DeepSeek ≈ ¥5 pour 1 000 appels ; les échecs ne sont pas facturés).',
}
