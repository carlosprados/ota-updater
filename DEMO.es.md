# DEMO — OTA updater en 30 minutos

Guía operativa para enseñar el sistema a compañeros. Mezcla narrativa
(lo que cuentas) con los comandos exactos (lo que ejecutas). Pensada para
leer de corrido en la pantalla secundaria mientras proyectas la principal,
o para improvisar sabiendo que todos los comandos exactos están aquí.

**Requisitos previos en la máquina**
- Go 1.22+ (probado con 1.25).
- [Task](https://taskfile.dev) instalado (`go install github.com/go-task/task/v3/cmd/task@latest`).
- Un navegador — mejor si puedes proyectarlo a pantalla grande.
- Bruno (opcional pero recomendado): GUI en <https://www.usebruno.com/downloads>, o CLI `npm i -g @usebruno/cli`.

**Duración**: 25–35 min con preguntas. Puedes cortar la parte de rollback
(paso 9) si vas justo de tiempo.

---

## 0 · El pitch de un minuto

> "Tenemos miles de dispositivos NB-IoT en campo. Actualizarlos es caro
> (la ventana de radio cuesta dinero y batería) y arriesgado (un binario
> corrupto puede dejar el dispositivo inalcanzable). Este sistema OTA
> resuelve las dos cosas: transmite sólo el **delta binario** firmado
> entre la versión vieja y la nueva (kilobytes en vez de megabytes),
> mantiene **dos slots A/B** para que el binario nuevo no pise al viejo
> hasta que demuestre que arranca sano, y si falla se **vuelve solo** a
> la versión anterior. Todo con un **servidor único** y un **agente que
> se puede embeber como librería Go** en cualquier ejecutable."

---

## 1 · Arquitectura

```text
                                            fsnotify
                            target.bin  ─────────────┐
                               │                     │
                               │                     ▼
  ┌───────────────────┐    POST /heartbeat    ┌───────────────────┐
  │                   │───────────────────────▶                   │
  │   edge-agent      │    GET /delta/A/B     │   update-server   │
  │   (device)        │◀──────────────────────│   (datacenter)    │
  │                   │    POST /report       │                   │
  └───────────────────┘───────────────────────▶                   │
       │                                      └───────────────────┘
       │ a/b swap + watchdog                        │  │
       ▼                                            │  │  /metrics
   ┌───┴───┐                                        │  │  (prometheus)
   │ slot A│ ◀─── active symlink                    │  │
   │ slot B│                                        │  │  /admin/reload
   └───────┘                                        │  │  /admin/loglevel
                                                    │  │  (bearer token)
                                                    ▼  ▼
                                               operator / CI / ops
```

**Puertos que usa la demo (todos loopback)**:

| Puerto | Componente | Rol |
|---|---|---|
| `127.0.0.1:18080` | update-server | API OTA: `/heartbeat`, `/delta/{from}/{to}`, `/report`, `/health`, `/admin/*` |
| `127.0.0.1:15683` | update-server | CoAP UDP (disponible pero no se ejercita en esta demo) |
| `127.0.0.1:19100` | update-server | Observabilidad: `/metrics` (Prometheus) + `/debug/pprof/*` |
| `127.0.0.1:7000`  | demo app | Página HTML de la propia "aplicación" desplegada — es lo que cambia visualmente al actualizarse |

**Qué es cada binario**:

- `update-server` — corre en el datacenter. Almacena binarios y deltas, firma manifests con Ed25519, sirve HTTP y CoAP.
- `demo/apps/v{1,2,3}/` — tres versiones del *mismo producto* de ejemplo. Cada binario es un servidor HTTP en `:7000` con una página visible distinta (colores + texto + endpoints) **Y embebe `pkg/agent`** como librería para auto-actualizarse. Esto es clave: **el auto-update no es un proceso separado, es parte de tu propia aplicación**.

---

## 2 · Instalación de Bruno (opcional, 2 min)

Bruno es un cliente de APIs tipo Postman pero con archivos-de-texto que
viven junto al código. La colección ya está en `bruno/` del repo.

**GUI (recomendada para la demo)**:
1. Descarga desde <https://www.usebruno.com/downloads>.
2. Abre Bruno → _Open Collection_ → elige la carpeta `bruno/` de este repo.
3. En la barra superior, selecciona el environment "**demo**".
4. Click en el icono de engranaje del environment → pestaña _Secrets_ →
   rellena `admin_token` con:

   ```
   demo-token-please-replace-in-prod-0000
   ```

   (el mismo valor que hay en `demo/configs/server.yaml`).

**CLI (si no quieres GUI)**: `npm i -g @usebruno/cli` y luego
`bru run bruno/public/health.bru --env demo`.

---

## 3 · Setup (una vez antes de empezar)

Compila los binarios, genera claves Ed25519, deja el estado inicial listo:

```sh
./demo/setup.sh
```

Esto crea `/tmp/ota-demo/` con:
- `update-server` compilado,
- `apps/v1`, `apps/v2`, `apps/v3` compilados,
- `keys/server.key` + `keys/agent.pub`,
- `target.bin` apuntando a v1,
- `agent/slots/A` y `agent/slots/B` sembrados con v1,
- `agent/current` symlink → `slots/A`.

Para limpiarlo todo en cualquier momento: `./demo/cleanup.sh`.

---

## 4 · Arrancar el server (terminal 1, ~2 min)

```sh
./demo/run-server.sh
```

Verás en los logs:

```
store opened target_hash=... target_in_memory=true ...
target watcher started dir=/tmp/ota-demo file=target.bin ...
coap listening addr=127.0.0.1:15683
http listening addr=127.0.0.1:18080
observability listening addr=127.0.0.1:19100 pprof=true
```

> **Qué contar aquí**: "El server acaba de leer el binario objetivo, ha
> calculado su SHA-256 y lo tiene en memoria. Ha abierto tres puertos:
> la API OTA, CoAP para dispositivos muy constreñidos, y un puerto de
> observabilidad separado para Prometheus. Nunca vuelve a mezclarse la
> API pública con métricas."

Desde Bruno (o `curl`), comprueba salud:

```sh
curl -s http://127.0.0.1:18080/health
```

**En Bruno**: `public/health` → _Run_.

```json
{"status":"ok","target_hash":"3c03ef60..."}
```

---

## 5 · Arrancar la demo app (terminal 2, ~2 min)

```sh
./demo/run-app.sh
```

El script ejecuta `/tmp/ota-demo/agent/current` (symlink → `slots/A`,
que es el binario v1). Verás:

```
demo banner HTTP listening op=demo version=1.0.0 addr=127.0.0.1:7000
demo app ready version=1.0.0 device_id=demo-device-01
no pending update; entering steady state
```

> **Qué contar**: "Lo que ha arrancado **no** es el agente por separado,
> es nuestra aplicación de ejemplo. Lleva dentro la librería `pkg/agent`.
> En un caso real esto sería vuestro binario de producción: una pasarela
> IoT, un agente de telemetría, un lector. El auto-update va embebido,
> no es infraestructura que haya que instalar aparte."

**Abre en el navegador y proyecta**: <http://127.0.0.1:7000/>

Pantalla azul. Texto grande "v1.0.0 — Initial release". La página hace
refresh cada 2 s (mira el `<meta http-equiv="refresh">`).

Cada 5 s verás en el log del agente:

```
heartbeat served from=<hash> to=<hash> update_available=false
```

> "El dispositivo pregunta al server cada 5 s si hay novedad. Como
> está en el mismo hash que el target, la respuesta es 'nada'. En
> producción este intervalo es 1 hora, con ±30 % de jitter para
> evitar que 5.000 dispositivos choquen al mismo segundo."

---

## 6 · Publicar v1.1.0 (actualización minor, ~3 min)

En un **tercer** terminal (el server y la app siguen corriendo):

```sh
./demo/publish-version.sh v2
```

Esto sobrescribe `/tmp/ota-demo/target.bin` con el binario v1.1.0 vía un
`cp` + `mv` atómico.

**En los logs del server (terminal 1)** verás casi al instante:

```
target event event=RENAME name=target.bin
store target reloaded previous_hash=<old> target_hash=<new> target_in_memory=true
```

> "El fsnotify del server ha visto el fichero cambiar, ha leído el nuevo
> binario, calculado su SHA-256 y sustituido el target. Cualquier
> heartbeat a partir de ahora recibirá el manifest nuevo."

**En los logs del agente (terminal 2)**, en el siguiente ciclo (máx 5 s):

```
heartbeat served update_available=true retry_after=5          # server genera delta
delta not yet ready retry_after=5
...
download complete path=/tmp/ota-demo/agent/slots/.staging.delta size=... transport=http
inactive slot written slot=B path=/tmp/ota-demo/agent/slots/B
active slot swapped new_active=B target=/tmp/ota-demo/agent/slots/B
update applied; restarting exec=/tmp/ota-demo/agent/slots/B
# ← aquí el proceso se reemplaza vía syscall.Exec, el PID es el mismo
demo banner HTTP listening op=demo version=1.1.0 addr=127.0.0.1:7000
post-swap boot detected; entering watchdog window
health check passed attempt=1 of=3
watchdog confirmed healthy boot
update confirmed; steady state engaged
```

**Mira el navegador**: la página pasa de azul a **verde**, el h1 dice
**v1.1.0**, y aparece un badge "NEW — try GET /hello".

Prueba el nuevo endpoint:

```sh
curl http://127.0.0.1:7000/hello
# Hello from v1.1.0! Endpoint introduced in the 1.1.0 release.
```

> **Lo que acaba de pasar en 10 líneas**:
> 1. El server vio el nuevo target via fsnotify.
> 2. El agente hizo heartbeat, recibió "actualización disponible".
> 3. El server aún no tenía el delta → respondió `RetryAfter=5`.
> 4. En segundo plano el server corrió `bsdiff` + `zstd` entre v1.0.0 y v1.1.0.
> 5. Siguiente heartbeat: server firma el manifest con Ed25519 y lo devuelve.
> 6. Agente VERIFICA la firma **antes de descargar** (si fuera falsa, aborta con 0 bytes descargados).
> 7. Descarga el delta comprimido (~250 KiB entre dos Go binarios de ~11 MiB).
> 8. Aplica bsdiff + zstd → binario nuevo → lo escribe al slot inactivo (B).
> 9. Intercambia el symlink → B ahora es activo.
> 10. Escribe `.pending_update`, hace `syscall.Exec` con el nuevo binario → el proceso se reemplaza manteniendo PID y conexiones abiertas, el HTTP `:7000` arranca del nuevo código.
> 11. El binario nuevo arranca, detecta `.pending_update`, hace un heartbeat de "salud", **lo confirma** → fija el estado.

---

## 7 · Major bump v2.0.0 (otros 3 min)

```sh
./demo/publish-version.sh v3
```

Misma secuencia. Pantalla a **rojo**, h1 "v2.0.0", nuevo endpoint
`/status`:

```sh
curl -s http://127.0.0.1:7000/status | python -m json.tool
```

```json
{
  "go": "go1.25.6",
  "hostname": "imperator",
  "introduced": "2.0.0",
  "pid": 620739,
  "uptime_secs": 12,
  "version": "2.0.0"
}
```

Nótese que **el PID no ha cambiado** en toda la demo — es el mismo
proceso reemplazado tres veces. Compara con el inicio.

---

## 8 · Observabilidad (3 min)

**En Bruno**: `metrics/scrape` → _Run_. O con curl:

```sh
curl -s http://127.0.0.1:19100/metrics | grep '^updater_' | sort
```

Series que merece explicar sobre la marcha:

```
updater_heartbeats_total{result="update",transport="http"}        # 2
updater_deltas_served_total{hot_hit="hit",transport="http"}        # 2
updater_delta_generations_total{result="ok"}                       # 2
updater_heartbeat_duration_seconds_count{transport="http"}         # N
updater_hot_delta_cache_bytes                                      # 5e5..
updater_target_binary_size_bytes                                   # ~11e6
updater_target_in_memory                                           # 1
```

> "Cada línea tiene semántica operativa clara. Si
> `updater_async_generations_inflight` se queda alto mucho tiempo en
> producción, algo está pidiendo bsdiff sin parar y hay que mirarlo.
> Si `deltas_served_total{hot_hit='miss'}` predomina sobre `hit`, la
> hot cache está infradimensionada."

Si quieres enseñar `pprof`:

```sh
go tool pprof -http=: http://127.0.0.1:19100/debug/pprof/goroutine
```

(Abre una UI en un puerto local con el grafo de goroutines.)

---

## 9 · Bonus: rollback simulado (3 min, opcional)

Para mostrar el watchdog salvando al dispositivo: corrompe el target
antes de publicarlo para que el proceso nuevo no arranque:

```sh
# sabotaje: publicamos un "binario" que no es ejecutable
echo 'not a binary' > /tmp/ota-demo/target.bin
chmod -x /tmp/ota-demo/target.bin
```

En el siguiente ciclo:

- El server le sirve un manifest con target_hash = hash del texto "not a binary\n".
- El agente descarga el "delta", lo aplica (error de bsdiff → hash mismatch → **aborta sin hacer swap**).

Para una demo más visual, en vez de eso puedes mostrar que **antes** de
descargar el agente verifica la firma del manifest. Si el server
devuelve un manifest manipulado (p. ej. deltaHash incorrecto firmado con
otra clave), `crypto.Verify` falla y el agente no descarga **ni un byte**.

> "Ese es el valor real: el agente nunca gasta radio en bytes que no
> autenticó antes. Es la decisión de diseño explícita que nos ahorra
> downlink NB-IoT cuando algo va mal en el pipeline."

Para restaurar y seguir jugando:

```sh
./demo/publish-version.sh v1   # vuelves al azul inicial
```

---

## 10 · Cerrar

En los terminales 1 y 2: `Ctrl+C`. Los logs muestran la secuencia de
shutdown limpia:

```
shutdown requested
target watcher stopping
store closed cleanly
shutdown complete
```

Para limpiar por completo el estado (borra `/tmp/ota-demo/`):

```sh
./demo/cleanup.sh
```

---

## Apéndice · FAQ que suelen salir

**¿Por qué no HTTPS?**
Está por diseño: los dispositivos NB-IoT a veces no tienen recursos para
TLS. La **autenticidad** del binario va por la firma Ed25519 del
manifest, no por el canal. La **confidencialidad** no es un requisito
aquí: un atacante sólo ve bytes comprimidos y firmados que no puede
modificar sin invalidar la firma.

**¿Por qué bsdiff y no algo "más moderno"?**
Evaluamos librsync y es ~100× peor en deltas reales de binarios Go. Ver
`benchmark/` del repo para los números. xdelta3 es el siguiente candidato
si necesitamos targets > 100 MiB.

**¿Y si el dispositivo se queda sin reloj?**
El heartbeat lleva `timestamp` pero el server **no lo valida**. Es
informativo. La autenticidad y la integridad viven en la firma del
manifest, no en el timestamp.

**¿Qué pasa si se cae el agente a mitad de una descarga?**
El `.partial` se guarda en disco con su estado JSON. Al siguiente
arranque, HTTP reanuda con `Range` desde el byte correcto. CoAP en
este release reinicia desde 0 (limitación conocida en README).

**¿Cuántos dispositivos puede aguantar este server?**
Objetivo declarado: **miles** de agentes NB-IoT. La RAM está acotada
(caches con LRU configurables), las goroutines async trackeadas, y el
admin tiene rate-limit. En steady state el server consume ~20 MiB de
RSS. Para decenas de miles haría falta un segundo replica + shared
storage; no es el caso actual.

**¿Se puede embeber `pkg/agent` en un binario que no es mío?**
Sí — exactamente eso es lo que acabas de ver con las apps v1/v2/v3. Lee
la sección "Embedding as a library" del `README.md` para el ejemplo
completo.
