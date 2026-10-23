export default {
  protocolHelp:
    'Wählen Sie das API-Format, nicht den Anbieter. DeepSeek, OpenRouter und eigene Gateways funktionieren mit einem kompatiblen Protokoll. Verwenden Sie deren Basis-URL, API-Schlüssel und Modell-ID.',
  compatibleProtocol: 'Kompatibles Protokoll',
  selectProtocol: 'API-Protokoll wählen',
  quickSetup: 'Schnelle Einrichtung',
  customGateway: 'Eigenes Gateway',
  modelId: 'Modell-ID',
  availableModels: 'Verfügbare Modelle',
  fetchModels: 'Modelle abrufen',
  modelsUnavailable: 'Modellliste nicht verfügbar; Modell-ID manuell eingeben.',
  title: 'AI-Anbieter',
  subtitle:
    'Pipewright trainiert keine eigenen Modelle – binde dein eigenes LLM für die Fehlerdiagnose und Konfigurationserstellung ein. Schlüssel liegen ausschließlich im verschlüsselten Tresor dieser Instanz und verlassen ihn nie.',
  statusConfigured: 'Konfiguriert',
  statusUnconfigured: 'Nicht konfiguriert',
  retry: 'Erneut versuchen',

  providerClaudeTag: 'Empfohlen für Diagnose',

  guidanceAria: 'AI-Konfigurationsleitfaden',
  guidanceTitle: 'Konfiguriere ein LLM, um die AI-Diagnose freizuschalten',
  guidanceBody:
    'Sobald du Claude, OpenAI oder einen OpenAI-kompatiblen Endpunkt (z. B. Ollama /v1) verbindest, generiert Pipewright bei einem Pipeline-Fehler automatisch Ursachenhypothesen und Korrekturvorschläge – ohne manuelles Durchsuchen der Logs.',

  selectProvider: 'Anbieter auswählen',
  providerRadioAria: 'AI-Anbieterauswahl',
  selectProviderAria: '{name} auswählen',
  providerConfig: '{name}-Konfiguration',
  lastSaved: 'Zuletzt gespeichert {time}',

  apiKeyHint:
    'Nach dem Speichern wird nur ein maskierter Wert angezeigt; leer lassen, um den vorhandenen Schlüssel zu behalten',
  apiKeyReplacing: 'Wird ersetzt…',
  apiKeyConfigured: 'Konfiguriert •••• (leer lassen = unverändert)',
  apiKeyPaste: 'API Key einfügen…',
  apiKeyMaskedAria: 'Konfigurierte Maske: {masked}',


  baseUrlLabel: 'Base URL',
  baseUrlHint: 'Standard: {url} (unverändert angehängt – Versionspräfixe wie /v1 beibehalten)',

  modelLabel: 'Modell',
  modelHint:
    'Hauptmodell für die Diagnose, z. B. claude-opus-4-7 / gpt-4o / llama3',

  testConnection: 'Verbindung testen',
  testOk: 'Inferenz OK · Latenz {ms}ms',
  testFail: 'Verbindung fehlgeschlagen',

  budgetLabel: 'Monatliches Token-Limit',
  budgetHint:
    'Pausiert die AI-Diagnose bei Überschreitung (leer = unbegrenzt; in diesem Zyklus nur deklariert, im nächsten Epic erzwungen)',
  budgetPlaceholder: 'z. B. 500000, leer = unbegrenzt',

  ctxTitle: 'Kontextverwaltung',
  ctxWindowHint:
    'Kontextbudget des Ops-Agenten: Nahe am Fensterlimit wird der alte Verlauf automatisch zu einer Zusammenfassung komprimiert; bei Overflow wird automatisch komprimiert und erneut versucht. Einheit: Tokens, leer = Standardwert.',
  ctxWindowLabel: 'Kontextfenster',
  ctxWindowPlaceholder: 'Standard 200000',
  ctxMaxOutputLabel: 'Max. Ausgabe pro Aufruf',
  ctxMaxOutputPlaceholder: 'Standard 8192',
  ctxKeepLabel: 'Beim Komprimieren behalten',
  ctxKeepPlaceholder: 'Standard 15000',
  ctxAutoCompact: 'Verlauf nahe am Limit automatisch komprimieren',
  ctxAdvanced: 'Erweitert',
  ctxBufferLabel: 'Auslöse-Reserve',
  ctxBufferPlaceholder: 'Automatisch (10 % des Fensters)',
  ctxHistoryMessagesLabel: 'Max. Verlaufsnachrichten',
  ctxHistoryMessagesPlaceholder: 'Standard 100',
  ctxHistoryBytesLabel: 'Max. Verlauf (KB)',
  ctxHistoryBytesPlaceholder: 'Standard 256',
  ctxNote:
    'Die Komprimierung betrifft nur den an das Modell gesendeten Kontext; die Sitzungsansicht zeigt den vollständigen Verlauf. Zusammenfassungen werden vom aktuell konfigurierten Modell erzeugt.',
  errContextInvalid: 'Kontextwerte müssen nicht-negative Ganzzahlen oder leer sein',

  enableAi: 'AI-Funktionen aktivieren',
  enableAiDesc:
    'Im ausgeschalteten Zustand wird die AI-Diagnose still übersprungen und die zentralen CI/CD-Pipelines bleiben unberührt',

  dirtyNote: 'Es gibt ungespeicherte Änderungen',
  cleanNote: 'Keine Änderungen',
  discard: 'Verwerfen',
  saveChanges: 'Änderungen speichern',

  toastSaveSuccess: 'AI-Einstellungen gespeichert',
  toastSaveFailed: 'Speichern fehlgeschlagen',

  errServerUnreachable:
    'Server nicht erreichbar. Prüfe, ob das Backend läuft, und versuche es erneut.',
  errServerUnreachableShort: 'Server nicht erreichbar',
  errVaultUnconfigured:
    'Im Tresor ist kein Master Key konfiguriert. Setze die Umgebungsvariable PIPEWRIGHT_MASTER_KEY.',
  errLoadFailed: 'Laden fehlgeschlagen ({status})',
  errLoadGeneric:
    'AI-Einstellungen konnten nicht geladen werden. Bitte später erneut versuchen.',
  errBudgetInvalid:
    'Das monatliche Token-Limit muss eine positive Ganzzahl oder leer sein',
  errProviderInvalid: 'Bitte einen gültigen Anbieter auswählen',
  errBaseUrlRequired: 'Bitte die Base URL eingeben',
  errApiKeyRequired:
    'API Key darf nicht leer sein (für lokale Endpunkte genügt ein beliebiger Wert)',
  errRequestFailed: 'Anfrage fehlgeschlagen ({status})',
  errUnknown: 'Unbekannter Fehler',
  webTitle: 'Websuche & Abruf',
  webEnable: 'Websuche/Abruf-Werkzeuge aktivieren',
  webEnableDesc: 'Stellt dem Ops-Agenten web_search / web_fetch bereit (nutzungsbasiert abgerechnet, standardmäßig aus)',
  webProviderLabel: 'Anbieter',
  webProviderHint: 'Der Anbietertyp bestimmt API-Dialekt und offizielle Standard-Base-URL; aktuell DeepSeek / GLM / Tavily / Bocha / Ollama; künftige Anbieter werden hier erweitert',
  webBaseUrlLabel: 'Base-URL',
  webBaseUrlHint: 'Leer lassen, um die LLM-Base-URL zu verwenden; ausfüllen zum Überschreiben (offizieller Standard: {url})',
  webBaseUrlDefaultHint: 'Leer lassen für die offizielle Standardadresse: {url}',
  webApiKeyHint: 'Leer lassen, um bei gleicher Base-URL den LLM-Schlüssel zu nutzen; ein Wert gilt nur für Web-Werkzeuge, danach maskiert',
  webTest: 'Suche testen',
  webAvailable: 'Web-Werkzeuge bereit (Ziel und Schlüssel aufgelöst)',
  webUnavailable: 'Aktuelle Konfiguration ist nicht verwendbar',
  webNote: 'web_search durchsucht das Web; web_fetch liest eine Seite als Markdown (von Bocha nicht angeboten). Agent-Aufrufe werden pro gewähltem Anbieter abgerechnet (fehlgeschlagene Aufrufe meist kostenlos).',
}
