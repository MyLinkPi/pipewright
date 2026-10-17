/**
 * serverCapacity — resumen de capacidad de una línea al elegir máquina
 * (despliegue de la tienda de apps, selección de hosts de puerta de enlace…).
 * Las partes disponibles se unen con ' · '.
 */
export default {
  containers: '{running} en ejecución / {total} contenedores',
  noRuntime: 'sin runtime de contenedores',
  unreachable: 'inaccesible',
  load: 'carga {load} / {cores} núcleos',
  mem: 'mem {used} / {total} {unit}',
  disk: 'disco {pct}%',
}
