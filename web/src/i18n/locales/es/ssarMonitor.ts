export default {
  title: 'Monitorización de máquinas',
  subtitle:
    'Monitorización de servidores con ssar (fork propio): los colectores desplegados muestrean localmente, la plataforma almacena muestras periódicas para la vista general y cada máquina conserva 7 días de historial detallado para el análisis en profundidad',
  errLoadTitle: 'Error al cargar',

  binHeading: 'Paquetes ssar',
  binEmpty: 'Aún no hay paquetes en caché (GitHub Release: MyLinkPi/ssar)',
  binDownloading: 'Descargando paquetes…',
  binVersion: 'En caché {version} ({n} paquete(s))',
  binErr: 'La última descarga falló: {err}',
  binDownload: 'Descargar paquetes',
  binRedownload: 'Volver a descargar',
  installAll: 'Instalar / actualizar todo',

  installAllOk: '{n} servidor(es) instalados',
  installPartial: '{ok} con éxito, {fail} fallidos',
  installFailed: 'La solicitud de instalación falló',

  serversHeading: 'Servidores',
  colServer: 'Servidor',
  colStatus: 'Estado de ssar',
  colLastSample: 'Última muestra',
  colActions: 'Acciones',
  notInstalled: 'No instalado',
  collectorActive: 'Recopilando',
  collectorStopped: 'Colector detenido',
  install: 'Instalar',
  upgrade: 'Actualizar',

  detailHeading: '{name} · detalle de monitorización',
  minutesLabel: '{n} min',
  hoursLabel: '{n} h',
  daysLabel: '{n} d',

  chartCpu: 'CPU (jiffies/s, agregado de todas las CPU)',
  chartMem: 'Memoria usada % (incluye caché de páginas)',
  chartLoad: 'Carga y cola de ejecución',
  chartTcp: 'Conexiones y retransmisiones TCP',
  cpuUser: 'user',
  cpuSystem: 'system',
  cpuIowait: 'iowait',
  cpuSteal: 'steal',
  memUsed: 'Usada',
  load1: 'load1',
  runq: 'Cola de ejecución',
  tcpEstab: 'Establecidas',
  tcpRetrans: 'Retrans/s',

  drillView: 'Vista de detalle',
  drillMachineLocal:
    'El detalle lee el historial ssar local de la máquina (7 días por defecto); la columna de hora es la hora local de la máquina',
  drillTime: 'Hora (local de la máquina)',

  chartEmpty: 'Sin datos todavía',
  chartAria: 'Gráfico de líneas de tendencias de métricas',
}
