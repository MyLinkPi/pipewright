/**
 * serverCapacity — résumé de capacité sur une ligne lors du choix d'une machine
 * (déploiement depuis la boutique, sélection d'hôtes passerelle…). Les parties
 * disponibles sont jointes par ' · '.
 */
export default {
  containers: '{running} en cours / {total} conteneurs',
  noRuntime: 'pas de runtime de conteneurs',
  unreachable: 'inaccessible',
  load: 'charge {load} / {cores} cœurs',
  mem: 'mém. {used} / {total} {unit}',
  disk: 'disque {pct}%',
}
