# CLAUDE.md — ota-updater

Complementa a `~/.claude/CLAUDE.md` (no duplica git/idioma/estilo). Aplica a este proyecto.

## Estado al cierre de sesión (2026-04-17)

- Rama activa: `ota/bootstrap-protocol-crypto`. Working tree limpio en el último commit.
- **Pasos 1–18 completados.** Plan ejecutado de principio a fin.
- Móvido en paso 18: `internal/{agent,protocol,crypto,delta,compression}` → `pkg/...` para permitir importación externa. `internal/server` se queda donde está (sólo lo consume `cmd/update-server`).
- Decisiones cerradas durante esta serie de sesiones:
  1. **Watchdog N=3** (2026-04-17): tres reintentos de heartbeat dentro de `Update.WatchdogTimeout`; evita rollbacks espurios por transitorios NB-IoT.
  2. **Self-restart `syscall.Exec` default + `RestartStrategy` pluggable** (2026-04-17), con `ExitRestart` como alternativa para entornos con supervisor. Compatibilidad systemd/Docker documentada en `README.md` §"Self-restart after swap".
  3. **Firma sobre `targetHash || deltaHash` (opción B)** (2026-04-16). Ver `docs/signing.md`.
  4. **Sin DTLS por ahora**, fallback "preferred-with-one-shot no-sticky", CoAP block size 512 (2026-04-16).
- Próximas iteraciones (post-step-18): rate-limiting + métricas Prometheus en server (`project_server_scale.md`); CoAP Block2 resume en agent; clock-skew validation server-side; revisar alternativa `icedream/go-bsdiff`.

## Qué es

Sistema OTA en Go para dispositivos NB-IoT. Dos binarios:

- `edge-agent` — cliente en dispositivo: heartbeat, descarga de delta, slots A/B, watchdog, rollback.
- `update-server` — servidor: manifest, generación y cacheo de deltas, HTTP/REST + CoAP.

Especificación canónica: `prompt-ota-updater.md` en la raíz. Cualquier desviación debe justificarse y anotarse.

## Stack

- Go 1.22+ (build con `CGO_ENABLED=0`, ambos binarios estáticos).
- Module path: `github.com/amplia/ota-updater`.
- Dependencias previstas: `go-bsdiff` (delta), `klauspost/compress/zstd`, `plgd-dev/go-coap/v3`, `fxamacker/cbor/v2`, `gopkg.in/yaml.v3`. `crypto/ed25519` y `log/slog` de stdlib. Minimizar externas.

## Layout

```
cmd/{edge-agent,update-server}/main.go
pkg/{agent,protocol,crypto,delta,compression}/   # exported, importable as a library
internal/server/                                  # binary-only: only used by cmd/update-server
integration/                                      # //go:build integration end-to-end test
tools/keygen/
configs/{agent,server}.yaml
docs/signing.md                                   # authoritative signature reference
```

## Orden de implementación

Sigue los 18 pasos del `prompt-ota-updater.md §Implementation Order`. Cada paso debe **compilar y pasar tests** antes del siguiente. Estado actual al final de este fichero.

## Alcance extendido

- **El agente es librería Go embebible** en cualquier ejecutable del usuario (decisión 2026-04-16, materializada en paso 18). API pública en `pkg/agent/` con nombres exportados, logger inyectable, `HealthChecker`/`RestartStrategy`/`HWInfoFunc` pluggables, sin globales. `cmd/edge-agent/main.go` es el wrapper delgado de referencia (~190 líneas). Ejemplo de embedding documentado en README.md §"Embedding as a library", verificado que compila.
- **Escala objetivo del servidor: miles de agentes NB-IoT** (decisión 2026-04-16). Robustez no opcional. Patrones obligatorios: cache de manifests firmados, semáforo de generación de deltas, límite de body en handlers, middleware de panic recovery en HTTP y CoAP, timeouts estrictos en `http.Server`, graceful shutdown, logs en cada request. Rate-limiting y métricas anotadas como siguientes pasos. Ver memoria `project_server_scale.md`.

## Decisiones CoAP (agente + servidor)

Decididas 2026-04-16:

- **Sin DTLS de momento.** Sólo `coap://` (UDP plano, puerto 5683). Añadir `coaps://` con PSK queda como extensión futura si hace falta.
- **Fallback preferred-with-one-shot** en el agente: intenta el transport preferido; si falla durante un ciclo, reintenta UNA vez con el otro; el siguiente ciclo vuelve al preferido. No "sticky" al fallback.
- **Block size por defecto: 512 bytes** (RFC 7959). Configurable 16..1024. Razonable para NB-IoT sin arriesgar fragmentación IP.
- **Serialización CoAP: CBOR** (tags `cbor:"N,keyasint"` ya en `pkg/protocol/messages.go`). HTTP sigue con JSON. Servidor responde según content-type/accept.

## Decisiones de proyecto

- **Firma Ed25519 sobre `targetHash || deltaHash`** (opción B, decidida 2026-04-16). El payload canónico lo construye `protocol.ManifestSigningPayload`. Permite al agente abortar una descarga corrupta antes de parchear (ahorra downlink NB-IoT), sin renunciar a la autenticidad del binario activado. Coste: firma por-par `(from,to)`, marginal con Ed25519. **Documentación autoritativa en [`docs/signing.md`](docs/signing.md)** — cualquier cambio que toque firmas debe actualizar ese fichero en el mismo commit.
- **Logging con `log/slog` (stdlib)**, nivel configurable y **cambiable en runtime** (decidido 2026-04-16). Config: `logging.level` (`debug|info|warn|error`) y `logging.format` (`text|json`). En runtime: `POST /admin/loglevel` con el mismo bearer token que `/admin/reload`. Niveles: DEBUG detalles internos, INFO operaciones normales, WARN anomalías recuperables, ERROR fallos. Campos obligatorios: `device_id`, `from`, `to`, `remote`, `op` en servidor; `device_id`, `version_hash`, `operation` en agente.
- **Self-restart del agente tras swap** (decidido 2026-04-17): `syscall.Exec` por defecto, detrás de una interfaz `RestartStrategy` pluggable. Se envía una segunda implementación lista `ExitRestart` para quien prefiera `os.Exit(0)` + relanzamiento del supervisor. Justificación: `syscall.Exec` mantiene PID, cgroup, env vars y FDs, es transparente para systemd (cualquier `Type=`, incluido `notify` reenviando `sd_notify(READY=1)` tras exec) y para Docker (PID 1 no cambia). Requisitos operativos: en Docker los slots A/B deben vivir en un volumen persistente; `ExecStart=` de systemd debe apuntar al symlink `current/edge-agent` (estable). Detalle completo en `README.md` §"Self-restart after swap".
- **Watchdog N=3** (decidido 2026-04-17): tres intentos de heartbeat dentro de `Update.WatchdogTimeout` antes de declarar fallo. Evita rollbacks espurios por transitorios NB-IoT. `HealthChecker` es una interfaz pluggable; default = heartbeat OK. Boot-count persistente en `<slotsDir>/.boot_count`; >2 arranques del mismo `version_hash` ⇒ versión marcada como mala, rollback permanente, reporte al server.
- **Pulido post-hardening** (PR-G, 2026-04-18): (item 17) `time.After` → `time.NewTimer + Reset` reusable en el `Run` loop del agente y en el retry loop del `Downloader`; cancelación de ctx libera recursos inmediatamente en vez de esperar al fire natural. (item 23) Negative cache TTL de 30 s + cap 256 entradas en `Store.HasBinary` — absorbe flood de hashes inválidos bajo ataque o con devices legacy. Hardcoded por decisión (no knob porque no hay intuición operacional). `RegisterBinary` y `Reload` hacen `invalidateMissCache` para visibilidad inmediata de binarios frescos. (item 24) Cooldown tras `ExecRestart` failure: se persiste `<StateDir>/.restart_cooldown` (JSON con wake-up Unix) vía `atomicio.WriteFile`; RunOnce short-circuitea silenciosamente mientras está activo. Configurable `update.restart_failure_cooldown` (default 1h). Triggers manuales (`TriggerUpdate()`, `.update_now`) lo saltan y lo limpian. (item 18) README sección nueva "Memory limits (device-side)" explicando `GOMEMLIMIT` (soft, GC-aggressive) + `MemoryMax=` systemd / `--memory` docker (hard, cgroup+OOM killer), con combo recomendado 80/20 y ejemplos unit file + docker-compose + k8s.
- **Observabilidad** (PR-F, 2026-04-18): Prometheus `/metrics` + `net/http/pprof` opcional en listener separado `metrics.addr` (default `127.0.0.1:9100`, loopback). Dependencia nueva `github.com/prometheus/client_golang`. `internal/server/metrics.go` — registry aislado por instancia (no `prometheus.DefaultRegisterer`), prefijo `updater_*`. Métricas expuestas: `heartbeats_total{transport,result}`, `heartbeat_duration_seconds{transport}`, `deltas_served_total{transport,hot_hit}`, `delta_serve_duration_seconds{transport}`, `delta_generations_total{result}`, `delta_generate_duration_seconds`, `async_generations_inflight`, `manifest_cache_entries`, `hot_delta_cache_bytes|entries`, `target_binary_size_bytes`, `target_in_memory`, `admin_requests_total{endpoint,code}`, `admin_rate_limited_total`, `signature_failures_total`. Label `code` acotado a `2xx/3xx/4xx/5xx` + explícitos `Unauthorized/Forbidden/Too Many Requests`. `Store.PeekHotDelta` permite al handler loguear hit/miss sin promover MRU. Disk-space warning al arrancar en server (`binaries_dir`+`deltas_dir`) y agent (`slots_dir`) usando `atomicio.Free` (`syscall.Statfs` bajo build-tag unix). Umbrales configurables `disk_space_min_free_pct` (10) + `disk_space_min_free_mb` (100); 0 desactiva. pprof default OFF; WARN al arrancar si ON.
- **Resiliencia red + disco** (PR-E, 2026-04-17→18): jitter configurable en `update.jitter` (default 0.3, ±30% cada ciclo) para evitar thundering herd horaria (item 6). `WriteTimeout` del server sube a 10 min para descargas NB-IoT grandes sin cortes artificiales (item 9). `pkg/atomicio.SweepStaleTemp` barre `.tmp-*`, `.partial` y similares >24h en `Open` del Store (binaries+deltas) y `NewUpdater` del agent (StateDir) — limpia residuos de procesos muertos mid-write (item 7). `pkg/atomicio.IsDiskFull` (ENOSPC Linux) permite al downloader aplicar backoff mínimo de 5 min en disk-full, evitando quemar retries en condición estática (item 11). `cmd/edge-agent` construye `http.Client` con `Transport` tuneado NB-IoT: Dial 30s, TLSHandshake 30s, ResponseHeader 60s, MaxIdleConns 2, IdleConnTimeout 90s (item 12). Agent HTTP client hace `drainAndClose(resp.Body)` antes de Close para reutilizar keep-alives (item 13). Token-bucket `golang.org/x/time/rate` en `bearerTokenMiddleware` sólo al fallar 401 (CI/CD con token correcto nunca throttleado), config `admin.rate_limit_per_sec` (5), `admin.rate_limit_burst` (20), 0 desactiva; responde 429 con `Retry-After: 1`. Cierra items 6, 7, 9, 11, 12, 13 y el pendiente post-16 del informe 24/7.
- **Shutdown limpio del server** (PR-D, 2026-04-17): `Store.Close(ctx)` espera via `sync.WaitGroup` a todas las goroutines async de `StartDeltaGeneration` (bsdiff no es ctx-cancellable). El watcher reescrito ejecuta `onChange` dentro de la goroutine de `Run` (sin `time.AfterFunc`) → cancelación de ctx garantiza que no hay callbacks en vuelo. `cmd/update-server/main.go` orquesta el shutdown: HTTP.Shutdown + CoAP.Stop → `store.Close(shutdownCtx)` → wait watcher goroutine, todo bounded por `cfg.HTTP.ShutdownTimeout` (default 15s). Admin token validado con longitud mínima de 32 caracteres en `Config.validate`, mensaje de error sugiere `openssl rand -hex 16`. Cierra items 8, 10, 15, 16 del informe 24/7. Rate limit del middleware admin queda fuera de scope (pendiente).
- **Durabilidad de escrituras** (PR-C, 2026-04-17): todas las escrituras atómicas pasan por `pkg/atomicio` (`WriteFile`, `WriteReader`, `ReplaceSymlink`). Cada una garantiza: `f.Sync()` del contenido, `Rename` atómico, y `fsync(dir)` del parent para que el dirent persista en power-loss. Callers unificados: `internal/server/store.go` (binarios + deltas), `pkg/agent/slots.go` (slot binario + symlink activo), `pkg/agent/updater.go` (pending_update), `pkg/agent/watchdog.go` (boot_count), `pkg/agent/downloader.go` (state JSON de resume — item 14 del informe 24/7). Política de `fsync(dir)` fallido: log warn + continuar (Linux siempre lo soporta; test-friendly para otros OS). Cierra items 4, 5 y 14 del informe 24/7.
- **Gate de auto-update por semver** (decidido 2026-04-17, PR-B): el agente compara su propia versión baked-in (inyectada con `-ldflags="-X main.version=..."` desde `cmd/edge-agent` o el binario embebedor) con `ManifestResponse.TargetVersion`. Dos knobs en `update.*`: `auto_update: bool` (master switch, default `true`) + `max_bump: none|patch|minor|major` (cap, default `major`). Versiones no-semver se gobiernan por `update.unknown_version_policy: deny|allow` (default `deny`). Dos triggers manuales equivalentes y one-shot para saltar el gate: fichero sidecar `<StateDir>/.update_now` (ops-friendly, constante `agent.UpdateNowFile`) y `Updater.TriggerUpdate()` (library API). Ambos se consumen tras un ciclo. `Heartbeat.Version string` añadido (CBOR tag 5, omitempty) para observabilidad. Dependencia nueva `golang.org/x/mod/semver`. Documentación autoritativa en `README.md` §"Update policy".
- **Claves**: PKCS#8 PEM para privada (`server.key`, 0600), PKIX PEM para pública (`agent.pub`, 0644). Generadas con `go run ./tools/keygen -out <dir>`.
- **`keygen` se niega a sobrescribir** ficheros existentes (O_EXCL). Destruir claves es manual y explícito.
- **Tags duales JSON + CBOR** en `pkg/protocol/messages.go` con claves CBOR enteras (compactas). Un único tipo por mensaje para HTTP y CoAP.
- **`ManifestResponse.RetryAfter`** añadido respecto al spec original (incoherencia detectada: §5.5 lo mencionaba pero §1 no lo definía).

## Riesgos abiertos (anotados del estudio del spec)

Pendientes de confirmación o decisión de diseño antes de llegar al paso correspondiente:

1. ~~**RAM de `bsdiff` en server**~~ — resuelto 2026-04-17 (PR-A). Ya NO se cachean binarios source en RAM (kernel page cache asume el rol). Target activo en RAM sólo bajo `store.target_max_memory_mb` (default 200 MiB); por encima, read-on-demand. Hot LRU de deltas en RAM con `store.hot_delta_cache_mb` (default 512 MiB) que absorbe campañas (miles de devices pidiendo el mismo delta → 1 disco read via singleflight). Manifest LRU con `manifest.cache_size` (default 4096). Límite duro identificado: bsdiff es ~20× el binario en pico; targets >100 MiB no son prácticos y librsync se descartó como sustituto tras benchmark (deltas ~100× más grandes sobre binarios Go reales). Ver `README.md` §"Memory bounds" y `benchmark/`.
2. ~~**Self-restart del agente tras swap**~~ — resuelto 2026-04-17: `syscall.Exec` default + `RestartStrategy` pluggable (`ExitRestart` como alternativa). Compatibilidad systemd/Docker documentada en README.
3. ~~**Watchdog N reintentos dentro de la ventana**~~ — resuelto 2026-04-17: **N=3** configurable. `HealthChecker` pluggable. Boot-count `<slotsDir>/.boot_count`; >2 ⇒ versión mala + rollback permanente.
4. ~~**Protección de delta corrupto**~~ — resuelto 2026-04-16: opción B implementada (firma sobre `targetHash || deltaHash`). Ver `pkg/protocol/signing.go`.
5. **Clock skew** en `Timestamp` de heartbeat/report: definir política de validación server-side.
6. **go-bsdiff**: poco activo. Validar temprano con binarios reales; considerar alternativa `icedream/go-bsdiff`.

## Comandos habituales

Build tool: **Taskfile** (`Taskfile.yml`), no Makefile. Instalar `task` (taskfile.dev) una vez.

```bash
task                    # listar tareas
task build              # ambos binarios (static, CGO_ENABLED=0)
task build-agent        # bin/edge-agent
task build-server       # bin/update-server
task keygen             # ./keys/{server.key,agent.pub}
task test               # go test ./... -race -count=1
task vet                # go vet ./...
task check              # vet + build (rápido, sin tests)
task ci                 # vet + test + build
task clean              # rm bin/ y store/deltas/
```

## Convenciones locales

- Paths de recurso (HTTP y CoAP) idénticos — definidos en `pkg/protocol/constants.go`. Handlers de transporte **mirror**.
- Logging: `slog` con campos estructurados. Campos obligatorios en agente: `device_id`, `version_hash`, `operation`. En servidor: `device_id`, `op`.
- Errores: siempre `fmt.Errorf("operación: %w", err)`. Sin `errors.New` en rutas de error con contexto útil.
- Rama de trabajo actual: `ota/bootstrap-protocol-crypto` (cortada desde `main`). Seguir convención `ota/<feature>` para las siguientes.

## Estado

- [x] Paso 1 — `pkg/protocol/` (messages + constants)
- [x] Paso 2 — `pkg/crypto/` + `tools/keygen/`
- [x] Paso 3 — `pkg/compression/`
- [x] Paso 4 — `pkg/delta/` (round-trip test ya incluido)
- [x] Paso 5 — `internal/server/store.go` (con tests: round-trip, cache hit, concurrent dedup, HasBinary, StartDeltaGeneration async)
- [x] Paso 6 — `internal/server/manifest.go` (con tests: target current, unknown source, delta cached con firma verificada, delta async)
- [x] Paso 7 — `internal/server/http_handler.go` (con tests: heartbeat 3 caminos, delta full/Range/404+async/traversal, report, health) + `docs/signing.md`
- [x] Paso 8 — `internal/server/coap_handler.go` (go-coap v3, CBOR, Block2 auto; tests: heartbeat current/cached+firma, delta full/404+async, report)
- [x] Paso 9 — `internal/server/config.go` + `cmd/update-server/main.go` (reinicio no permitido; `Store.Reload` con `RWMutex`, watcher fsnotify con debounce, `POST /admin/reload` con bearer estático, `LevelVar` + `POST /admin/loglevel`, graceful shutdown SIGINT/SIGTERM, timeouts estrictos, `configs/server.yaml` de ejemplo)
- [x] Paso 10 — `pkg/agent/config.go` + `configs/agent.yaml` (tipos exportados para uso librería; `Transport` type-safe; `ApplyDefaults`/`Validate` públicos; tests incluyen flujo library-no-YAML)
- [x] Paso 11 — `pkg/agent/slots.go` (SlotManager A/B con Active/Inactive/WriteToInactive/Swap/Rollback; symlink swap atómico via tmp+rename; tests cubren active/inactive, atomicity, swap/rollback, inactive intact, validaciones)
- [x] Paso 12 — `pkg/agent/downloader.go` (`DeltaTransport` interface + `HTTPTransport` con Range + `CoAPTransport` sin resume; Downloader con state JSON, exp backoff+jitter, verify SHA-256 final, fallback a offset=0 cuando transport rechaza resume)
- [x] Paso 13 — `pkg/agent/watchdog.go` + `restart.go` (HealthChecker pluggable con DefaultHealthChecker basado en HeartbeatFunc; BootCounter con persistencia atómica en `<slotsDir>/.boot_count`, JSON, idempotente; Watchdog con `WaitForHealth` N=3 reintentos en ventana, `CheckBoot` que devuelve `ErrBootCountExceeded` cuando count>MaxBoots=2, `Confirm` que limpia el contador; `RestartStrategy` interface + `ExecRestart` (default, `syscall.Exec`, env preservado) + `ExitRestart` (alternativa con código configurable). Tests: persistencia entre instancias, escalada del contador, ventana con N intentos, helper de subproceso para validar `syscall.Exec` real)
- [x] Paso 14 — `pkg/agent/updater.go` + `client.go` + `client_http.go` + `client_coap.go` (`ProtocolClient` interfaz + impls HTTP/JSON y CoAP/CBOR; `ClientPair` que valida coherencia client/transport; `Updater` orquestador con `Run` (boot + loop) y `RunOnce`; `BootPhase` lee `.pending_update`, ejecuta `Watchdog.CheckBoot`/`WaitForHealth`/`Confirm` y reporta éxito o hace rollback+report+exec; `RunOnce` sigue `docs/signing.md §5` exacto: heartbeat → verify ANTES de descargar → download via `Downloader` → patch+verify reconstrucción → write `.pending_update` → swap → exec; fallback "preferred-with-one-shot" no-sticky entre primary y fallback ClientPair; `RestartStrategy` inyectable; `HWInfoFunc` pluggable. Tests: validación de deps, no-update/RetryAfter short-circuit, signature falsa aborta antes de descargar, happy path completo (download+patch+swap+pending+restart capturado), fallback de heartbeat reseteado por ciclo, ambos transports fallan, BootPhase sin pending, BootPhase mismatch limpia marker, BootPhase healthy confirma+reporta+limpia, BootPhase health falla rollback+report+exec, BootPhase boot count exceeded rollback permanente, HTTP client round-trip JSON con httptest, CoAP client URL/scheme/dial-failure)
- [x] Paso 15 — `cmd/edge-agent/main.go` + `pkg/agent/logging.go` (wrapper fino sobre `agent.Updater`: carga config, logger con `LevelVar`, public key, `SlotManager`, `BootCounter`, par primario y opcional fallback según `transport`/`fallback` del YAML, `HealthChecker` con `HeartbeatFunc` ligado al primary client, `ExecRestart` por defecto, `signal.NotifyContext(SIGINT,SIGTERM)`, `updater.Run(ctx)` con shutdown graceful. `task build-agent` y `task ci` re-habilitados; ambos binarios compilan estáticos)
- [x] Paso 16 — gap-fill de tests unitarios (cobertura `pkg/agent` 73.2% → 77.0%; gaps cerrados: CoAP `Report` round-trip con servidor in-process, `Updater.reportUpdate` con/sin fallback (primary OK, primary fail+fallback OK, ambos fallan no-fatal), `Updater.RunOnce` con `RestartStrategy` que falla → rollback + clearPending, `mismatchedPairError.Error()`. Skips deliberados anotados: getters `Name()`, `defaultHWInfo`, `NewLogger` (vs `NewLoggerTo`), errores fsync/chmod en writeLocked, errores internos go-coap)
- [x] Paso 17 — `integration/integration_test.go` con `//go:build integration`. Ubicado en subdir top-level (no en root) para no contaminar `go list ./...` con un package raíz vacío. Boota update-server real in-process (httptest), middleware que captura `UpdateReport` POSTs, registra el oldBin del agent en el Store del server, monta agent real (SlotManager+BootCounter+Watchdog+ClientPair HTTP+Updater), drives RunOnce hasta el restart capturado, valida bytes-exact reconstrucción de slot B + symlink + pending marker; luego invoca BootPhase manualmente jugando el rol del binario nuevo, valida watchdog Confirm, pending limpiado, boot count reseteado y UpdateReport recibido por el server con success=true y hashes correctos. Nuevo `task test-integration` separado del `task ci` por velocidad y aislamiento de CI ligero
- [x] Paso 18 — promoción a librería + README final. `git mv internal/{agent,protocol,crypto,delta,compression} → pkg/` (server queda en `internal/` por ser binary-only); todos los imports reescritos en `cmd/edge-agent`, `cmd/update-server`, `internal/server`, `integration/`, `tools/keygen` y los propios packages movidos. Sección "Embedding as a library" en README.md con ejemplo Go autocontenido (verificado que compila contra los packages reales). `docs/signing.md` y `CLAUDE.md` actualizados a los nuevos paths. Bloque "Layout" del CLAUDE.md actualizado. `task ci` + `task test-integration` siguen verdes tras el move.
