/**
 * serverCapacity — einzeilige Kapazitätsübersicht bei der Maschinenwahl
 * (App-Store-Deployment, Gateway-Hostauswahl…). Verfügbare Teile werden
 * mit ' · ' verbunden.
 */
export default {
  containers: '{running} laufend / {total} Container',
  noRuntime: 'keine Container-Runtime',
  unreachable: 'nicht erreichbar',
  load: 'Last {load} / {cores} Kerne',
  mem: 'RAM {used} / {total} {unit}',
  disk: 'Disk {pct}%',
}
