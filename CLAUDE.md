# CLAUDE.md — ota-updater

Complementa a `~/.claude/CLAUDE.md` (no duplica git/idioma/estilo). Aplica a este proyecto.

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
internal/{protocol,crypto,delta,compression,agent,server}/
tools/keygen/
configs/{agent,server}.yaml
```

## Orden de implementación

Sigue los 18 pasos del `prompt-ota-updater.md §Implementation Order`. Cada paso debe **compilar y pasar tests** antes del siguiente. Estado actual al final de este fichero.

## Decisiones de proyecto

- **Firma Ed25519 del hash del binario target** (no del delta). Permite cambiar de ruta de delta sin re-firmar. Ver `internal/crypto/`.
- **Claves**: PKCS#8 PEM para privada (`server.key`, 0600), PKIX PEM para pública (`agent.pub`, 0644). Generadas con `go run ./tools/keygen -out <dir>`.
- **`keygen` se niega a sobrescribir** ficheros existentes (O_EXCL). Destruir claves es manual y explícito.
- **Tags duales JSON + CBOR** en `internal/protocol/messages.go` con claves CBOR enteras (compactas). Un único tipo por mensaje para HTTP y CoAP.
- **`ManifestResponse.RetryAfter`** añadido respecto al spec original (incoherencia detectada: §5.5 lo mencionaba pero §1 no lo definía).

## Riesgos abiertos (anotados del estudio del spec)

Pendientes de confirmación o decisión de diseño antes de llegar al paso correspondiente:

1. **RAM de `bsdiff` en server** — cachear agresivo, considerar precómputo en arranque.
2. **Self-restart del agente tras swap** — `syscall.Exec` puro vs. dependencia de systemd. Decidir antes del paso 14 (`updater.go`).
3. **Watchdog**: criterio "alcanza server" es frágil en NB-IoT. Exigir N reintentos durante la ventana, no fallo instantáneo. Decidir antes del paso 13.
4. **Protección de delta corrupto**: considerar incluir `DeltaHash` en el payload firmado del manifest para abortar antes de gastar ancho de banda parcheando. Decidir antes del paso 6 (`manifest.go`).
5. **Clock skew** en `Timestamp` de heartbeat/report: definir política de validación server-side.
6. **go-bsdiff**: poco activo. Validar temprano con binarios reales; considerar alternativa `icedream/go-bsdiff`.

## Comandos habituales

```bash
make build-agent        # bin/edge-agent (static)
make build-server       # bin/update-server (static)
make keygen             # genera ./keys/{server.key,agent.pub}
make test               # go test ./... -v -race
go build ./... && go vet ./...   # verificación rápida
```

## Convenciones locales

- Paths de recurso (HTTP y CoAP) idénticos — definidos en `internal/protocol/constants.go`. Handlers de transporte **mirror**.
- Logging: `slog` con campos estructurados. Campos obligatorios en agente: `device_id`, `version_hash`, `operation`. En servidor: `device_id`, `op`.
- Errores: siempre `fmt.Errorf("operación: %w", err)`. Sin `errors.New` en rutas de error con contexto útil.
- Rama de trabajo actual: `ota/bootstrap-protocol-crypto` (cortada desde `main`). Seguir convención `ota/<feature>` para las siguientes.

## Estado

- [x] Paso 1 — `internal/protocol/` (messages + constants)
- [x] Paso 2 — `internal/crypto/` + `tools/keygen/`
- [x] Paso 3 — `internal/compression/`
- [x] Paso 4 — `internal/delta/` (round-trip test ya incluido)
- [ ] Paso 5 — `internal/server/store.go`
- [ ] Paso 6 — `internal/server/manifest.go`
- [ ] Paso 7 — `internal/server/http_handler.go`
- [ ] Paso 8 — `internal/server/coap_handler.go`
- [ ] Paso 9 — `internal/server/config.go` + `cmd/update-server/main.go`
- [ ] Paso 10 — `internal/agent/config.go`
- [ ] Paso 11 — `internal/agent/slots.go`
- [ ] Paso 12 — `internal/agent/downloader.go`
- [ ] Paso 13 — `internal/agent/watchdog.go`
- [ ] Paso 14 — `internal/agent/updater.go`
- [ ] Paso 15 — `cmd/edge-agent/main.go`
- [ ] Paso 16 — tests unitarios
- [ ] Paso 17 — test de integración (`integration_test.go`, tag `integration`)
- [ ] Paso 18 — Makefile, README, configs de ejemplo
