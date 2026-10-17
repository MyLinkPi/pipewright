/**
 * serverCapacity — one-line capacity summary shown next to machines in pickers
 * (app-store deploy, gateway host selection…). Parts are joined by ' · ' based
 * on data availability.
 */
export default {
  containers: '{running} running / {total} containers',
  noRuntime: 'no container runtime',
  unreachable: 'unreachable',
  load: 'load {load} / {cores} cores',
  mem: 'mem {used} / {total} {unit}',
  disk: 'disk {pct}%',
}
