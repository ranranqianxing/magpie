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
| What an inference id means | A bare route id, a variant string (`id[context=…,reasoning=…,fast=…]`) or a legacy slug (`id-200k-high-fast`) names that catalog entry. The same match is used for the id on `AgentService/Run`. Anything else is Cursor's own model. | [`route.go`](../../internal/cursoride/route.go), [`agentrun.go`](../../internal/cursoride/agentrun.go) |
| The gateway request | An invoke's messages, or an Agent run's `conversation_state` plus the current user text, become a `/v1/chat/completions` body: `model` is the route id, `reasoning_effort` is the level Cursor sent, `stream` is true. Tools on the invoke are copied. An Agent run also offers the built-in composer tools and any MCP tool the run defined. | [`compile.go`](../../internal/cursoride/compile.go), [`history.go`](../../internal/cursoride/history.go), [`tools.go`](../../internal/cursoride/tools.go) |
| The catalog HTTP call | `Handler` answers the five paths above. When an upstream reply is supplied it appends magpie's rows; otherwise it answers with those rows alone. | [`handler.go`](../../internal/cursoride/handler.go) |
| The proxy | Listens on `127.0.0.1:47331`. CONNECT to a `*.cursor.sh` host is terminated with a certificate this package's CA signed. Other CONNECT traffic is tunneled. A local run's text, thinking, and tool-call deltas are streamed back as inference frames, or as Agent composer frames (including `exec_server_message`) when the call is `AgentService/Run`. | [`proxy.go`](../../internal/cursoride/proxy.go), [`reply.go`](../../internal/cursoride/reply.go), [`agentrun.go`](../../internal/cursoride/agentrun.go), [`tools.go`](../../internal/cursoride/tools.go), [`ca.go`](../../internal/cursoride/ca.go) |
| The Agents row | Id `cursor-ide`, name Cursor. Connect writes Cursor's proxy settings, starts the proxy, and installs the CA into the user trust store when the installed copy is missing, expired, or not this file. A write of those settings then quits Cursor and opens it again. Disconnect puts the previous settings back and stops the proxy. It does not remove the CA and it does not quit Cursor. | [`cursoride.go`](../../internal/agent/cursoride.go) |
| Diagnostics | One timestamped line per catalog inject, route hit or miss, chat start and finish, TLS host, and CA match or replace. The same text is the file beside the CA and the Diagnostics block on the opened Cursor row. | [`diag.go`](../../internal/cursoride/diag.go) |

`Handler` is not mounted on the gateway listener (`:3425`). The gateway
speaks the agent APIs in [gateway routing](gateway-routing.md). Ordinary
Cursor does not call those. The proxy calls the gateway's
`/v1/chat/completions` with `gateway.TokenFor("cursor-ide")`. Nothing here
writes `state.vscdb`, Cursor's Open configuration, or
`CURSOR_LOCAL_AGENT_BASE_URL`. The CA is written under magpie's config
(`cursor-ide/ca.crt`). Connect installs that certificate into the user
trust store (`TrustCA` in [`trust.go`](../../internal/cursoride/trust.go)).
It does not write the machine-wide store and does not ask for an
administrator.

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
2. Connect writes `http.proxy`, `http.proxyKerberosServicePrincipal`, `http.proxySupport` (`on`), `cursor.general.disableHttp2` (`true`) and `http.experimental.systemCertificatesV2` (`true`) into Cursor's `settings.json`, and deletes `http.noProxy` while connected. The previous raw values are stashed and put back on disconnect, comments included. It then listens on `127.0.0.1:47331` and checks the user trust store for this file's certificate. Already means the store holds this file's fingerprint and the file is inside its validity window: the install is skipped, and the row says the certificate is already trusted. A magpie CA with another fingerprint, or this file when it is outside that window, is removed and this file is installed (an expired file on disk is replaced first, so the install is a new certificate, not the expired one). A failure, or the Linux system-bundle command, is that same notice's one remaining step, and connect still succeeds. When the write changed `settings.json`, magpie quits ordinary Cursor and opens it again, on macOS, Windows, and Linux. The notice then says Cursor was opened again. When Cursor cannot be found, or the quit fails, the notice still says to quit and reopen it, and connect still succeeds. A second connect that already matches does not rewrite the file and does not quit Cursor; it runs the trust check again. Disconnect does not remove the CA and does not quit Cursor. `KeepCursorIDE` starts the proxy again when magpie serves and the settings already match. It does not write settings, does not open a trust prompt, and does not quit Cursor.
3. Cursor, restarted so it reads those settings, sends `CONNECT` for its hosts. The proxy terminates TLS for `*.cursor.sh` and tunnels anything else. `POST` `/aiserver.v1.AiService/AvailableModels` (and the usable-model and default-model paths) goes to `Handler`. `Models` is `ForAgent("cursor-ide")`, read on each request.
4. `Handler` asks Cursor's own backend for the same call on the list and usable-model paths. A 2xx reply is decoded as a Connect unary frame when it is one, and magpie's protobuf is appended. A failed upstream answers with magpie's rows only. A compressed frame is a 502. `GetDefaultModel`, `GetDefaultModelForCli`, and `GetDefaultModelNudgeData` do not append: a 2xx upstream reply is returned as Cursor sent it, so a catalog id is not installed as the IDE default. A failed upstream on those three still answers with the local stand-in.
5. `RunInferenceSSE` waits until `BidiAppend` for the same request id says whether the run is local. A gzipped append (`Content-Encoding: gzip` or `x-gzip`, or a gzip body) is decompressed first so that decision can be read. A run whose conversation id is a UUID and whose model is in the catalog is local: the proxy sends `run_ready` (`vendor` `unknown`, `prompt_version` `latest`) and answers the invoke from the gateway's chat stream (text and thinking frames, then an invocation end). `Finish` sends the Connect end-of-stream frame. Cursor's own model, a `data_binary` append, and a client message that is not this inference run are forwarded. The forwarded body is the decompressed bytes: `Content-Encoding` is omitted and `Content-Length` is the length of those bytes. A request this package does not decompress, including a catalog call, is forwarded with the encoding the client sent. The agent protocol's run request is not a UUID in that field, so a `BidiAppend` that carries it is forwarded.
6. `POST` `/agent.v1.AgentService/Run` is the Agent composer. Its `requested_model.model_id` is the catalog id the picker stored (`codex/gpt-5.6-luna`, `workbuddy/deepseek-v4.1-flash`, `aicodings-openai/gpt-6-astra`, or that id plus a variant string or `reasoning` parameter). Cursor's own backend answers `ERROR_BAD_MODEL_NAME` / `Unknown model ID` for every such id, which is why the picker can list the model while the send fails. A catalog model is not forwarded. The chat is the current user text plus `conversation_state`: each `root_prompt_messages_json` entry that is JSON, or a blob id whose bytes are in `pre_fetched_blobs`, becomes a gateway message (text, tool call, tool result). A blob id with no bytes is counted in the log and left out. The gateway stream comes back as `interaction_update` text and thinking deltas. A tool-call delta becomes `partial_tool_call`, then `tool_call_started` and `exec_server_message` (shell, read, grep, delete, ls, write, edit, or MCP), then `turn_ended`, then the Connect end frame. A stream that ends without `finish_reason` or `[DONE]` still closes a tool call it already started. The same close happens on the inference path. Cursor's own model on this path is still forwarded. A route miss is logged as a forward. It is not logged as `Unknown model ID`, which is what Cursor's backend says only when a catalog id is forwarded. A gzipped connect frame is read decompressed; the forwarded body of a Cursor model stays the bytes the client sent, aside from an HTTP gzip that was stripped so the length matches. A truncated `conversation_state` is refused. It is not forwarded and not treated as an empty history.
7. The chat `POST` uses `Authorization: Bearer <gateway.TokenFor("cursor-ide")>` and `X-Magpie-Session` from `Chat.Header` when the run named a conversation. The token is not written into the protobuf or into Cursor's settings.

A catalog request larger than 32 MiB is refused. It is not read as empty, which would replace Cursor's list with magpie's rows alone.

`settings.json` also records `magpie.cursorGateway` (the gateway address the proxy calls) and, on sync, `magpie.cursorModels` (the catalog ids last published). Cursor ignores both. The proxy does not read them: it calls `gateway.URL()` and `ForAgent` on each request. The gateway key is what drift and a port move look at, including a reverse proxy in front of magpie. The model key is what a catalog sync refreshes without putting an address the user pointed elsewhere back on this machine.

## What the chat body contains

`model` is the catalog id. `messages` are the invoke's system, user,
assistant and tool turns, or, on `AgentService/Run`, those recovered from
`conversation_state` with the current user text appended when that
typed prompt is not already the last user turn. A stored user turn
keeps the surrounding `user_info`; the action is only what was typed.
A text turn is a string. A turn with an image is an OpenAI content
array; the image is a data URL. A file that is not text is a note that
names its media type, not the bytes. Tool calls keep their
id, name and raw arguments. `tools` is set when the invoke named tools, and
on every Agent run (the built-in composer tools, plus MCP tools the run
defined). `max_tokens` is set when the invoke's model config named an output
cap. `reasoning_effort` is set when the picker named a level, including
`none`. `stream` is true.

`Fast` and `ContextTokens` are kept on `Route` and `Chat` and are not
written into the JSON. The gateway's `max_tokens` is an output cap, not
Cursor's context choice, and the gateway has no field for the fast switch.

## Failure

A truncated protobuf is an error from `Decide`, `ReadClient` and
`CompileChat`. It is not an empty run and not a request to forward. An
unknown inference role is an error, so a role this package does not know is
not sent on as a user turn. A context or fast value that does not parse is
an error only when the model id is magpie's. The same garbage on a Cursor
model is ignored, because that run is forwarded. An `AgentService/Run`
body that does not parse is forwarded. A truncated `conversation_state`
on a run that does parse is refused. One that names a catalog model and
then a parameter that does not parse is refused, the same way the
inference path refuses it. Tool arguments that are not JSON are not sent
as an exec. The composer is told, in a text frame, that the arguments
did not parse.

An empty catalog encodes no default model. Cursor is not given a stand-in.

## Verification

`go test ./internal/cursoride` covers the picker fields, the route ids, the
chat body, the catalog HTTP replies, the CA files, a CONNECT that injects
a catalog row, a local run whose gateway stream comes back as text and
thinking frames, a gzipped BidiAppend forwarded without `Content-Encoding`,
a gzipped catalog-model run that still finishes as text, an
`AgentService/Run` whose model id is `aicodings-openai/gpt-6-astra`,
`codex/gpt-5.6-luna` or `workbuddy/deepseek-v4.1-flash` (the gateway is
asked for that catalog id, and Cursor's own model on the same path is
still forwarded), a run whose `conversation_state` is replayed into the
chat messages, a gateway tool-call delta that comes back as an Agent
exec frame and as an inference `tool_call_part`, an Agent stream that
ends without `finish_reason` and still execs the call, and a
default-model reply that keeps Cursor's own id when the backend answered.
`go test ./internal/agent -run CursorDesktop` covers the settings stash, a
second connect that does not rewrite, drift when the recorded gateway
address is swapped, a hidden model left out of the list, the notice for a
trust step that is still left, the notice when the certificate is
already trusted, and a relaunch that runs only after a connect writes
settings. The relaunch in that test is a hook. A Linux test starts a
copy of `sleep` and checks that binary is stopped. Neither test opens
the installed Cursor or a trust store.

The same lines are written to `cursor-ide/cursor-ide.log` next to
`ca.crt` (`DiagPath`). Installed, that is
`~/.config/magpie/cursor-ide/cursor-ide.log`
(`$XDG_CONFIG_HOME/magpie/cursor-ide/cursor-ide.log` when that is set;
the same path under the user's folder on Windows, which is not
`%APPDATA%\magpie` — `%APPDATA%` is where Cursor's own `settings.json`
lives; a portable run uses the `data` folder beside magpie). The opened
Cursor row on the Agents page shows those lines, with Refresh and Copy
all. `GET /api/agents/cursor-ide/log` returns the path and the same text.

To try it in the IDE, connect the Cursor row on the Agents page (id
`cursor-ide`). Cursor CLI is the row magpie cannot connect. Magpie writes
a local CA to `cursor-ide/ca.crt` in its config directory
(`appdir.Config`). Installed, the file is
`~/.config/magpie/cursor-ide/ca.crt`
(`$XDG_CONFIG_HOME/magpie/cursor-ide/ca.crt` when that is set; the same
path under the user's folder on Windows, not `%APPDATA%\magpie`; a
portable run uses the `data` folder beside magpie). Connect then installs
it for this user when the store does not already hold this file inside
its dates:

- Windows: `certutil -user -store Root`, then `-delstore` for another magpie CA, then `-addstore` into Current User\Root when this file is missing. Windows shows one consent dialog for the add. It does not ask for an administrator.
- macOS: `security add-trusted-cert` into the login keychain, user trust settings, not the System keychain. macOS asks for the login keychain password, not an administrator.
- Linux: when `certutil` from libnss3-tools is installed, the user NSS database `~/.pki/nssdb` is updated with no prompt. Cursor's Node stack reads the system bundle, which this install does not write. The notice gives one command (`update-ca-certificates`, `update-ca-trust`, or `trust anchor`) for that bundle. magpie does not run it.

If the prompt was dismissed, the notice on the row says so and names the certificate. Disconnect and connect the row again to show the prompt once more. A connect that finds this file's fingerprint already trusted, and the file still inside its dates, skips the install and says so on the row. A different magpie certificate in that store is removed first. Disconnect does not remove the certificate. A connect that changed Cursor's settings quits Cursor and opens it again; leave magpie running, and pick a magpie model in Cursor's model menu. When that relaunch does not run, the notice still says to quit and reopen Cursor. A model hidden for this row on the Agents page is not in that menu. A chat on that model should finish. Open the Cursor row and read Diagnostics: catalog, route, chat, TLS, and CA lines, the same text as `cursor-ide.log` beside the CA. On the Agent composer the run is
`AgentService/Run`; a catalog id is answered by the gateway, so Cursor's
backend is not asked to accept `provider/model`. A gzipped append is no
longer forwarded as gzip, which was the upstream `decompression failed`
that left the stream reconnecting. The running magpie has to be a build
that contains this proxy. An older binary still forwards `AgentService/Run`
and Windows Agent chat still shows `Unknown model ID`.

## Not this slice

Fast and the context choice are on the route and are not fields of the
chat body. There is no Hammers module in this repo; the gateway has no
equivalent of Cursor's fast switch, and its `max_tokens` is an output cap,
so those two picker axes are not invented as JSON. Blob ids that the run
did not prefetch are not fetched from Cursor's KV store. A tool result
that the IDE writes only after the response has started is not read back
on the same HTTP/1.1 body, so the model sees that result on the next run's
`conversation_state`, not inside the turn that emitted the exec. Append
sequence numbers are not reordered, and a run's session is not dropped
when the stream ends. HTTP/2 is not intercepted;
`cursor.general.disableHttp2` is set so Cursor uses HTTP/1.1 after it
restarts. The gateway still does not speak Cursor's protocol on `:3425`.
The machine-wide trust store is not written. On Linux the system bundle
stays a command in the notice, because writing it needs root. Disconnect
does not remove the CA and does not quit Cursor.
