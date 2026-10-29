# Cursor IDE model injection

Ordinary Cursor asks its own backend for the model list and sends the
chosen id on its inference protocol. Magpie's catalog is not in that list,
so picking a third-party model there is impossible until the list Cursor
sees contains magpie's rows and a later inference request for one of those
ids is answered by magpie's gateway.

Cursor Private Inference is a different product. It is the build whose
`product.json` says `Cursor Private Inference`, and it reads the gateway
from `CURSOR_LOCAL_AGENT_BASE_URL` and `CURSOR_LOCAL_AGENT_API_KEY`
(`cursorLocalOn` in [`cursorlocal.go`](../../internal/agent/cursorlocal.go)).
Magpie writes none of ordinary Cursor's settings for that row. This page
is the other path: ordinary Cursor, magpie's existing models.

## Responsibilities and boundaries

| Part | Responsibility | Source |
| --- | --- | --- |
| Rows Cursor lists | Build the protobuf appended to `AvailableModels`, `GetUsableModels`, `GetDefaultModel`, `GetDefaultModelForCli` and `GetDefaultModelNudgeData`. | [`catalog.go`](../../internal/cursoride/catalog.go) |
| Which models | The catalog that agent is allowed to show (`provider.CatalogFor`, via `ForAgent`). A model hidden on the Agents page is not injected. The display name is the model's name. The route id is the catalog id the gateway already routes (`provider/model`, or a group's id). | [`from.go`](../../internal/cursoride/from.go) |
| What an inference id means | A bare route id, a variant string (`id[context=…,reasoning=…,fast=…]`) or a legacy slug (`id-200k-high-fast`) names that catalog entry. Anything else is Cursor's own model. | [`route.go`](../../internal/cursoride/route.go) |
| The gateway request | An invoke's messages become a `/v1/chat/completions` body: `model` is the route id, `reasoning_effort` is the level Cursor sent, `stream` is true. | [`compile.go`](../../internal/cursoride/compile.go) |
| The catalog HTTP call | `Handler` answers the five paths above. When an upstream reply is supplied it appends magpie's rows; otherwise it answers with those rows alone. | [`handler.go`](../../internal/cursoride/handler.go) |
| The proxy | Listens on `127.0.0.1:47331`. CONNECT to a `*.cursor.sh` host is terminated with a certificate this package's CA signed. Other CONNECT traffic is tunneled. A local run's text and thinking are streamed back as inference frames. | [`proxy.go`](../../internal/cursoride/proxy.go), [`reply.go`](../../internal/cursoride/reply.go), [`ca.go`](../../internal/cursoride/ca.go) |
| The Agents row | Id `cursor-ide`, name Cursor. Connect writes Cursor's proxy settings and starts the proxy. Disconnect puts the previous settings back and stops the proxy. | [`cursoride.go`](../../internal/agent/cursoride.go) |

`Handler` is not mounted on the gateway listener (`:3425`). The gateway
speaks the agent APIs in [gateway routing](gateway-routing.md). Ordinary
Cursor does not call those. The proxy calls the gateway's
`/v1/chat/completions` with `gateway.TokenFor("cursor-ide")`. Nothing here
writes `state.vscdb`, Cursor's Open configuration, or
`CURSOR_LOCAL_AGENT_BASE_URL`. The CA is written under magpie's config
(`cursor-ide/ca.crt`) and is not installed into the system trust store.

## Field meanings

Cursor paints `client_display_name` as the model's name and
`model_picker_badges` as the blue capsule beside it. A capsule was where a
provider host or a group label showed up. Rows from this package set:

| Cursor field | Value |
| --- | --- |
| `name`, `server_model_name`, and each `model_names` entry | The catalog id. This is the id Cursor sends back and the gateway's `model`. |
| `client_display_name`, `inputbox_short_model_name` | The model's name (`Entry.Name`, else the vendor model id). Not `Entry.Label`, which appends the provider. |
| `model_picker_badges` | Absent. |
| `vendor` | `magpie`, for every row. A provider's own name is not copied: a provider added by its address is often named by that host. |
| credentials on a usable-model row | `cursor-magpie-local`, with no `base_url`. Not a provider key and not a gateway token (`magpie` / `magpie-<agent>`). The proxy routes by the model id. |

A model that thinks offers its catalog levels as the `reasoning` parameter
(`low`, `high`, …). One that thinks with no levels listed offers low,
medium and high. One that does not think has no effort axis. Context
choices are 200k, 356k, 500k, 800k and 1m, plus the model's own window when
that size is not already one of those. Fast is on the picker because
Cursor's variant id carries it (`fast=true` / `fast=false`); the chat body
does not grow a field for it.

The variant string and the legacy slug are the same id plus those
parameters, so a request that names either still resolves to the catalog id.

## Runtime path

1. The Agents page's Cursor row (`cursor-ide`) is connectable the way Claude Desktop is: its provider field is magpie, and `ListsModels` gives the row the same model menu. Cursor CLI stays the row that cannot be connected. Its `cli-config.json` has no place for a magpie model.
2. Connect writes `http.proxy`, `http.proxyKerberosServicePrincipal`, `http.proxySupport` (`on`), `cursor.general.disableHttp2` (`true`) and `http.experimental.systemCertificatesV2` (`true`) into Cursor's `settings.json`, and deletes `http.noProxy` while connected. The previous raw values are stashed and put back on disconnect, comments included. It then listens on `127.0.0.1:47331`. A second connect that already matches does not rewrite the file. `KeepCursorIDE` starts the proxy again when magpie serves and the settings already match, and does not write them.
3. Cursor, restarted so it reads those settings, sends `CONNECT` for its hosts. The proxy terminates TLS for `*.cursor.sh` and tunnels anything else. `POST` `/aiserver.v1.AiService/AvailableModels` (and the usable-model and default-model paths) goes to `Handler`. `Models` is `ForAgent("cursor-ide")`, read on each request.
4. `Handler` asks Cursor's own backend for the same call. A 2xx reply is decoded as a Connect unary frame when it is one, and magpie's protobuf is appended. A failed upstream answers with magpie's rows only. A compressed frame is a 502.
5. `RunInferenceSSE` waits until `BidiAppend` for the same request id says whether the run is local. A run whose conversation id is a UUID and whose model is in the catalog is local: the proxy sends `run_ready` (`vendor` `unknown`, `prompt_version` `latest`) and answers the invoke from the gateway's chat stream (text and thinking frames, then an invocation end). `Finish` sends the Connect end-of-stream frame. Cursor's own model, a `data_binary` append, and a client message that is not this inference run are forwarded. The agent protocol's run request is not a UUID in that field, so it is forwarded.
6. The chat `POST` uses `Authorization: Bearer <gateway.TokenFor("cursor-ide")>` and `X-Magpie-Session` from `Chat.Header` when the run named a conversation. The token is not written into the protobuf or into Cursor's settings.

A catalog request larger than 32 MiB is refused. It is not read as empty, which would replace Cursor's list with magpie's rows alone.

`settings.json` also records `magpie.cursorGateway` (the gateway address the proxy calls) and, on sync, `magpie.cursorModels` (the catalog ids last published). Cursor ignores both. The proxy does not read them: it calls `gateway.URL()` and `ForAgent` on each request. The gateway key is what drift and a port move look at, including a reverse proxy in front of magpie. The model key is what a catalog sync refreshes without putting an address the user pointed elsewhere back on this machine.

## What the chat body contains

`model` is the catalog id. `messages` are the invoke's system, user,
assistant and tool turns. A text turn is a string. A turn with an image is
an OpenAI content array; the image is a data URL. A file that is not text
is a note that names its media type, not the bytes. Tool calls keep their
id, name and raw arguments. `reasoning_effort` is set when the picker named
a level, including `none`. `stream` is true.

`Fast` and `ContextTokens` are kept on `Route` and `Chat` and are not
written into the JSON. The gateway has no one field that means Cursor's
fast switch or its context choice.

## Failure

A truncated protobuf is an error from `Decide`, `ReadClient` and
`CompileChat`. It is not an empty run and not a request to forward. An
unknown inference role is an error, so a role this package does not know is
not sent on as a user turn. A context or fast value that does not parse is
an error only when the model id is magpie's. The same garbage on a Cursor
model is ignored, because that run is forwarded.

An empty catalog encodes no default model. Cursor is not given a stand-in.

## Verification

`go test ./internal/cursoride` covers the picker fields, the route ids, the
chat body, the catalog HTTP replies, the CA files, a CONNECT that injects
a catalog row, and a local run whose gateway stream comes back as text and
thinking frames. `go test ./internal/agent -run CursorDesktop` covers the
settings stash, a second connect that does not rewrite, drift when the
recorded gateway address is swapped, and a hidden model left out of the
list. Neither opens Cursor.

To try it in the IDE: connect Cursor on the Agents page, trust
`cursor-ide/ca.crt` (the notice gives the path), quit Cursor and open it
again, leave magpie running, and pick a magpie model in Cursor's model
menu. A model hidden for this row on the Agents page is not in that menu.

## Not this slice

Tool-call deltas from the gateway are not turned into Cursor's tool
protocol. Fast and the context choice are on the route and are not fields
of the chat body. Append sequence numbers are not reordered, and a run's
session is not dropped when the stream ends. HTTP/2 is not intercepted;
`cursor.general.disableHttp2` is set so Cursor uses HTTP/1.1 after it
restarts. The gateway still does not speak Cursor's protocol on `:3425`.
Trusting the CA is a step the user does; magpie does not install it.
