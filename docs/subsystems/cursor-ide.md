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
is the other path: ordinary Cursor, magpie's existing models. What a
person using that path runs into is in
[Cursor 接入三方 API 经验交流](../../cursor接入三方api经验交流.md) (Chinese).
The facts below stay the contract.

## Responsibilities and boundaries

| Part | Responsibility | Source |
| --- | --- | --- |
| Rows Cursor lists | Build the protobuf appended to `AvailableModels`, `GetUsableModels`, `GetDefaultModel`, `GetDefaultModelForCli` and `GetDefaultModelNudgeData`. | [`catalog.go`](../../internal/cursoride/catalog.go) |
| Which models | The catalog that agent is allowed to show (`provider.CatalogFor`, via `ForAgent`). A model hidden on the Agents page is not injected. The display name is the model's name. The route id is the catalog id the gateway already routes (`provider/model`, or a group's id). | [`from.go`](../../internal/cursoride/from.go) |
| What an inference id means | A bare route id, a variant string (`id[context=…,reasoning=…,fast=…]`) or a legacy slug (`id-200k-high-fast`) names that catalog entry. The same match is used for the id on `AgentService/Run`. Anything else is Cursor's own model. | [`route.go`](../../internal/cursoride/route.go), [`agentrun.go`](../../internal/cursoride/agentrun.go) |
| The gateway request | An invoke's messages, or an Agent run's `conversation_state` plus the current user text and that turn's `selected_context`, become a `/v1/chat/completions` body: `model` is the route id, `reasoning_effort` is the level Cursor sent, `stream` is true. Tools on the invoke are copied. An Agent run also offers the built-in composer tools and any MCP tool the run defined. A follow-up `ExecClientMessage` is the tool result for the next round. | [`compile.go`](../../internal/cursoride/compile.go), [`history.go`](../../internal/cursoride/history.go), [`turns.go`](../../internal/cursoride/turns.go), [`attach.go`](../../internal/cursoride/attach.go), [`execresult.go`](../../internal/cursoride/execresult.go), [`tools.go`](../../internal/cursoride/tools.go) |
| The catalog HTTP call | `Handler` answers the five paths above. When an upstream reply is supplied it appends magpie's rows; otherwise it answers with those rows alone. | [`handler.go`](../../internal/cursoride/handler.go) |
| The proxy | Listens on `127.0.0.1:47331`. CONNECT to a `*.cursor.sh` host is terminated with a certificate this package's CA signed. Other CONNECT traffic is tunneled. A local run's text, thinking, and tool-call deltas are streamed back as inference frames, or as Agent composer frames (including `exec_server_message`) when the call is `AgentService/Run`. Agent runs loop on tool results read from later Connect request frames. | [`proxy.go`](../../internal/cursoride/proxy.go), [`agentloop.go`](../../internal/cursoride/agentloop.go), [`reply.go`](../../internal/cursoride/reply.go), [`agentrun.go`](../../internal/cursoride/agentrun.go), [`tools.go`](../../internal/cursoride/tools.go), [`ca.go`](../../internal/cursoride/ca.go) |
| The Agents row | Id `cursor-ide`, name Cursor. Connect writes Cursor's proxy settings, starts the proxy, and installs the CA into the user trust store when the installed copy is missing, expired, or not this file. A write of those settings then quits Cursor and opens it again. Disconnect puts the previous settings back, stops the proxy, and does not remove the CA. When that restore puts the previous settings back, it quits Cursor and opens it again through the same relaunch. A disconnect that leaves the file alone does not. | [`cursoride.go`](../../internal/agent/cursoride.go), [`cursoride_relaunch.go`](../../internal/agent/cursoride_relaunch.go) |
| Diagnostics | One timestamped line per catalog inject, route hit or miss, chat start and finish, TLS host, and CA match or replace. A connect or a disconnect that opens Cursor again also records `cursor relaunch opened` (or `cursor relaunch failed`, or `cursor relaunch skipped` when Cursor was not found). The same text is the file beside the CA and the Diagnostics block on the opened Cursor row. | [`diag.go`](../../internal/cursoride/diag.go), [`cursoride_relaunch.go`](../../internal/agent/cursoride_relaunch.go) |

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
2. Connect writes `http.proxy`, `http.proxyKerberosServicePrincipal`, `http.proxySupport` (`on`), `cursor.general.disableHttp2` (`true`) and `http.experimental.systemCertificatesV2` (`true`) into Cursor's `settings.json`, and deletes `http.noProxy` while connected. The previous raw values are stashed and put back on disconnect, comments included. It then listens on `127.0.0.1:47331` and checks the user trust store for this file's certificate. Already means the store holds this file's fingerprint and the file is inside its validity window: the install is skipped, and the row says the certificate is already trusted. A magpie CA with another fingerprint, or this file when it is outside that window, is removed and this file is installed (an expired file on disk is replaced first, so the install is a new certificate, not the expired one). A failure, or the Linux system-bundle command, is that same notice's one remaining step, and connect still succeeds. When the write changed `settings.json`, magpie quits ordinary Cursor and opens it again, on macOS, Windows, and Linux. The notice then says Cursor was opened again. When Cursor cannot be found, or the quit fails, the notice still says to quit and reopen it, and connect still succeeds. A second connect that already matches does not rewrite the file and does not quit Cursor; it runs the trust check again. Disconnect does not remove the CA. When the restore puts the previous settings back, it quits ordinary Cursor and opens it again, on macOS, Windows, and Linux, through the same relaunch a connect uses (`cursorIDEAfterWrite`). The notice then says Cursor was opened again, and the diagnostics log records `cursor relaunch opened`. When Cursor cannot be found, or the quit fails, the notice still says to quit and reopen it, and disconnect still succeeds. A disconnect that leaves the file alone does not quit Cursor. `KeepCursorIDE` starts the proxy again when magpie serves and the settings already match. It does not write settings, does not open a trust prompt, and does not quit Cursor.
3. Cursor, restarted so it reads those settings, sends `CONNECT` for its hosts. The proxy terminates TLS for `*.cursor.sh` and tunnels anything else. `POST` `/aiserver.v1.AiService/AvailableModels` (and the usable-model and default-model paths) goes to `Handler`. `Models` is `ForAgent("cursor-ide")`, read on each request.
4. `Handler` asks Cursor's own backend for the same call on the list and usable-model paths. A 2xx reply is decoded as a Connect unary frame when it is one, and magpie's protobuf is appended. A failed upstream answers with magpie's rows only. A compressed frame is a 502. `GetDefaultModel`, `GetDefaultModelForCli`, and `GetDefaultModelNudgeData` do not append: a 2xx upstream reply is returned as Cursor sent it, so a catalog id is not installed as the IDE default. A failed upstream on those three still answers with the local stand-in.
5. `RunInferenceSSE` waits until `BidiAppend` for the same request id says whether the run is local. A gzipped append (`Content-Encoding: gzip` or `x-gzip`, or a gzip body) is decompressed first so that decision can be read. A run whose conversation id is a UUID and whose model is in the catalog is local: the proxy sends `run_ready` (`vendor` `unknown`, `prompt_version` `latest`) and answers the invoke from the gateway's chat stream. Text and thinking go out as they arrive. Tool calls stay in the assembler until that body ends, including a `finish_reason` that is followed by a duplicate completion in the same body. The completing `tool_call_part` then carries the sealed arguments JSON and `is_complete`. Cursor's local loop concatenates until that flag and runs Pi Read from the result. No earlier fragment is sent, so a prefix is not concatenated with the object that replaces it. A call that is still missing the strings that tool validates is not sent. `where=seal`, `where=frames`, and `where=round` are logged on this path, next to `append` and `chat start path=BidiAppend`. `Finish` sends the Connect end-of-stream frame. Cursor's own model, a `data_binary` append, and a client message that is not this inference run are forwarded. The forwarded body is the decompressed bytes: `Content-Encoding` is omitted and `Content-Length` is the length of those bytes. A request this package does not decompress, including a catalog call, is forwarded with the encoding the client sent. The agent protocol's run request is not a UUID in that field, so a `BidiAppend` that carries it is forwarded.
6. `POST` `/agent.v1.AgentService/Run` is the Agent composer. Its `requested_model.model_id` is the catalog id the picker stored (`codex/gpt-5.6-luna`, `workbuddy/deepseek-v4.1-flash`, `aicodings-openai/gpt-6-astra`, or that id plus a variant string or `reasoning` parameter). Cursor's own backend answers `ERROR_BAD_MODEL_NAME` / `Unknown model ID` for every such id, which is why the picker can list the model while the send fails. A catalog model is not forwarded. The chat is the current user text plus `conversation_state`: each `root_prompt_messages_json` entry that is JSON, or a blob id whose bytes are in `pre_fetched_blobs`, becomes a gateway message (text, tool call, tool result, or an image part). A blob id with no bytes is counted in the log and left out. When that prompt is missing blobs, or it produced no messages, `turns` (field 8) is replayed instead if it yields more messages: each agent turn's user text and Pi tool steps (the call's args and its result) become the chat. A complete JSON prompt is left as it is; turns are not appended beside it. The current `UserMessage.selected_context` is rendered onto that user turn: an inline image (or a prefetched blob) becomes a data URL. Raw image bytes are base64-encoded once. Bytes that are already a data URL, or already the base64 of an image, are not encoded again. A selected image that names a path and does not carry bytes is read from that path when the file is a regular image of at most 8 MiB. The path is not logged. A path that cannot be read is counted missing, and the typed text is kept. Attached file text, code selections, extra context and attached rules are appended when that text is not already in the turn. A `selected_context` that does not parse is left out; the typed text is kept. The gateway stream comes back as `interaction_update` text and thinking deltas. A tool-call delta becomes `partial_tool_call`, then `tool_call_started` and `exec_server_message`. `partial_tool_call` field 3 is the aggregated arguments JSON so far, including when the newest gateway chunk was only a fragment, an arguments object, or an `index` of `0.0`. Cursor 3.27 runs the Pi arms: read (card 61, exec 45, `path`), shell (card 62, exec 46, `command`), edit (card 63, exec 47, `path` plus `old_string`/`new_string`), write (card 64, exec 48, `path` and `content`), grep (card 65, exec 49, `pattern`), glob (card 66, exec 50, `pattern`), and ls (card 67, exec 51, `path`). Delete stays exec field 4. MCP stays exec field 11. When the client sends another Connect frame on the same run — buffered in one body or on a chunked request the proxy reads while the response streams — carrying that tool result, the proxy appends it to the chat and asks the gateway again in the same turn. The result is the JSON tool message, or the executor's `ExecClientMessage` (AgentClientMessage field 2): `exec_id` (field 15) is the tool-call id, and Pi results are fields 46–52 (read, bash, edit, write, grep, find, ls). A shell stream chunk (field 14) is not that result, so the loop keeps waiting. A throw (`ExecClientControlMessage` field 2) is an error result for the exec's uint32 id. `agent exec result` logs the kind, whether it matched a call still open, and the byte and image counts. The text is not logged. A follow-up that is neither a result nor a heartbeat logs its field numbers only. A chunked run that is not a catalog model is routed before any follow-up reader starts; the forward carries the first frame and the rest of the body together. `turn_ended` and the Connect end frame go out only after the model stops without further tools. A stream that ends without `finish_reason` or `[DONE]` still closes a tool call it already started. The same close happens on the inference path. When no follow-up frame arrives, the turn still ends after the exec, as before. Cursor's own model on this path is still forwarded. A route miss is logged as a forward. It is not logged as `Unknown model ID`, which is what Cursor's backend says only when a catalog id is forwarded. A gzipped connect frame is read decompressed; the forwarded body of a Cursor model stays the bytes the client sent, aside from an HTTP gzip that was stripped so the length matches. A truncated `conversation_state` is refused. It is not forwarded and not treated as an empty history.
7. The chat `POST` uses `Authorization: Bearer <gateway.TokenFor("cursor-ide")>` and `X-Magpie-Session` from `Chat.Header` when the run named a conversation. The token is not written into the protobuf or into Cursor's settings.

A catalog request larger than 32 MiB is refused. It is not read as empty, which would replace Cursor's list with magpie's rows alone.

`settings.json` also records `magpie.cursorGateway` (the gateway address the proxy calls) and, on sync, `magpie.cursorModels` (the catalog ids last published). Cursor ignores both. The proxy does not read them: it calls `gateway.URL()` and `ForAgent` on each request. The gateway key is what drift and a port move look at, including a reverse proxy in front of magpie. The model key is what a catalog sync refreshes without putting an address the user pointed elsewhere back on this machine.

## What the chat body contains

`model` is the catalog id. `messages` are the invoke's system, user,
assistant and tool turns, or, on `AgentService/Run`, those recovered from
`conversation_state` with the current user text appended when that
typed prompt is not already the last user turn. A stored user turn
keeps the surrounding `user_info`; the action's typed text is appended
only when that turn is not already last. The same turn also receives
the action's `selected_context` (images, file text, code selections,
extra context, attached rules) when those bytes are not already in it.
A text turn is a string. A turn with an image is an OpenAI content
array; the image is a data URL. A history image is that URL when
`image` or `image_url` is a string, a data URL, or an object with
`url`, `data`, or `base64`. A file that is not text, on the
inference invoke path, is a note that names its media type, not the
bytes, unless those bytes sniff as an image: then the part is the
data URL. A part only labeled `image/*`, whose bytes are not an image,
stays the note. An Agent image is the data URL, not that note. A tool
result that is an array of text and image parts keeps the images on
the tool message. A history tool message takes its call id from the
part's `toolCallId`, or from the message's `toolCallId`, `tool_call_id`,
or `id`. The result body is `result`, or `output` when `result` is
absent. Tool calls keep their
id, name and raw arguments. A history `tool-call` part stores those
arguments on `args`. When that object is missing or `{}`, the same part's
`input`, `arguments`, or `parameters` object is used. A part with none of
those is replayed as `{}`. The first eight such parts in a replay each
log the tool name and the part's keys, and a further line counts the rest. `tools` is set when the invoke named tools, and
on every Agent run (the built-in composer tools, plus MCP tools the run
defined). Read's schema offers `path`, `offset`, and `limit`. `offset` is
the 1-indexed line to start at and `limit` is how many lines to return;
either may be omitted, and a read with neither is the whole file. On the
Pi read exec those are `PiReadExecArgs` field 2 and field 3, both optional
int32 (card 61, exec 45). Cursor 3.27.22's `cursor-agent-exec` declares
the same message as `PiReadExecArgs|1 path 9|2 offset 5?|3 limit 5?`, and
its `LocalPiReadExecutor` slices lines from `offset-1` for `limit` lines. Edit stays
`path`, `old_string`, and `new_string`. `max_tokens` is set when the invoke's model config named an output
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
did not parse. A chunk whose arguments are a JSON object, or whose
tool-call index is not an integer, is still folded into that call.
A first arguments chunk of `{}` is a placeholder, not a finished call.
Later fragments of that call replace it, and a sibling `parameters`,
`args`, or `input` object is kept when `arguments` is that placeholder.
A choice that carries the call twice keeps the arguments that have
fields: the delta's `""` or `{}` loses to the same choice's `message`
tool call when that one names the path (or the other string that tool
validates), whether that value is a JSON object or a string. Two
message calls that disagree are left as the delta had them. A later
complete object replaces an unfinished fragment that is not its
prefix, which a duplicate completion does when it resends the object
with different spacing. A finished object is not replaced by a shorter
one.
No partial frame is sent for the placeholder, so Cursor does not validate
the Pi strings until the real JSON is there. A call that is still empty
when the stream ends is filled from the same reply when that reply names
the tool: a DSML invoke, a `<tool_call>` JSON object, DeepSeek's
`{"name":…}` followed by `<parameter>` tags, or GLM's `<arg_key>` pairs.
`bash` fills `Shell`. Free prose is not read. A Pi call that is still
missing the strings that tool validates (Read's path, Shell's command,
and the same for Grep, Glob, Delete, Write, and Edit) is not executed, so
Cursor does not run `Read {}` and loop. The same skip is the local
inference loop: a BidiAppend invoke does not send a `tool_call_part`
for that call, so Cursor does not run `Read {}` from an empty
completing part either. LS may omit its path. An MCP
tool's JSON object, including `{}`, is still executed. One diagnostic
line per sealed call records the tool name, argument-fragment count, how
many of those fragments were `{}`, whether the sealed object is empty,
the argument key names, which markup form filled it (`message` when the
choice's message supplied the arguments), which of those forms
appeared in the text, and whether the exec ran. A second line, `where=frames`, has `exec=written` when the Pi
exec frames go out, or when the inference loop's completing
`tool_call_part` goes out, and `exec=skipped` when they do not. A round logs `where=round` with how many
calls were sealed and how many will run, including a round that sealed
none. Both entries log these lines: `AgentService/Run` inside
`agentGatewayRound`, and the BidiAppend invoke (`chat start
path=BidiAppend`) when its gateway body ends. A gateway request that
already carries assistant tool calls records how many of those arguments
are `{}`. Values are not logged. The same lines are the Cursor row's
Diagnostics and, when `MAGPIE_DEBUG=1`, the process log, each prefixed
`cursor-ide:`. An Agent run also logs `agent exec result` (kind,
matched, byte count, image count), `agent history source=turns` when
the structured turns replaced a prompt whose blobs were missing, and
`agent image source=path` or `agent image skipped` when a selected
image was a path. Those lines do not include the path, the image
bytes, or the tool result text. The route line includes `images=`,
the number of image parts on the chat.

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
chat messages, a current turn whose `selected_context` image and file
text are on that chat body, a history image part that stays an image
URL, a gateway tool-call delta that comes back as an Agent
exec frame and as an inference `tool_call_part`, a Write call whose
started card is `ToolCall` field 64 and whose exec is Pi field 48
with path and content, a Read exec on field 45 whose offset and limit
are fields 2 and 3 when the call names them, a shell exec on
field 46, a placeholder `{}` followed by argument fragments that still
execs the path, a Read whose arguments are a JSON object or a string
and whose Pi exec carries the path, a delta `{}` whose same choice's
message holds that object (sent twice) and which still execs the path,
a native `{}` filled from a DSML invoke of the same tool,
a native `{}` filled from `<parameter name="path">` after `{"name":"Read">`,
a Read whose history part stores the path on `input` rather than `args`,
a native `{}` whose only path is a sentence and which therefore does not exec,
a BidiAppend invoke (RunInferenceSSE) whose gateway body has a prefix,
then `finish_reason`, then a duplicate message tool call with the path,
and whose one completing `tool_call_part` carries that path while the
process log shows `where=seal`, `where=frames`, and `where=round` beside
`append` and `chat start path=BidiAppend`, the same invoke filled from
`<parameter name="path">`, and a prose sentence that does not send a
Read part,
an Agent stream
that ends without `finish_reason` and still execs the call, a same-turn
tool loop when a second Connect request frame carries the tool result,
a same-turn loop whose follow-up is a Pi read `ExecClientMessage`
(a shell-stream chunk before it is not the result, and the result text
is not in the diagnostic line), a throw matched by the exec's uint32
id, a history tool message whose call id is the message `id` and whose
body is `output`, a history `image_url` string and an image object
that stay data URLs, a tool result that keeps an image part, a
`conversation_state` whose root prompt is a missing blob and whose
`turns` replay the Read call and its result (a complete root prompt is
not replaced by those turns), a selected image whose field is already
base64 and is not encoded again, a selected image named only by a path
that is read into a data URL (the path is not logged), an inference
file part whose bytes sniff as a PNG and becomes a data URL while a
part merely labeled `image/png` stays the media-type note, and a
default-model reply that keeps Cursor's own id when the backend answered.
`go test ./internal/agent -run CursorDesktop` covers the settings stash, a
second connect that does not rewrite, drift when the recorded gateway
address is swapped, a hidden model left out of the list, the notice for a
trust step that is still left, the notice when the certificate is
already trusted, and a relaunch that runs after a connect writes
settings and after a disconnect restores them, and does not run when
that disconnect leaves the file alone. The relaunch in that test is a hook. A Linux test starts a
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

If the prompt was dismissed, the notice on the row says so and names the certificate. Disconnect and connect the row again to show the prompt once more. A connect that finds this file's fingerprint already trusted, and the file still inside its dates, skips the install and says so on the row. A different magpie certificate in that store is removed first. Disconnect does not remove the certificate. A connect that changed Cursor's settings quits Cursor and opens it again; leave magpie running, and pick a magpie model in Cursor's model menu. When that relaunch does not run, the notice still says to quit and reopen Cursor. A disconnect that put the previous settings back quits Cursor and opens it again the same way. The notice says Cursor was opened again, and Diagnostics records the same `cursor relaunch opened` line. When that relaunch does not run, the notice still says to quit and reopen Cursor. A disconnect that leaves the file alone does not quit Cursor. A model hidden for this row on the Agents page is not in that menu. A chat on that model should finish. Open the Cursor row and read Diagnostics: catalog, route, chat, TLS, and CA lines, the same text as `cursor-ide.log` beside the CA. On the Agent composer the run is
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
so those two picker axes are not invented as JSON. `RequestContext`
(always-on rules, env, the `file_contents` map) is not copied; a rule
the user attached on `selected_context` is. Blob ids that the run
did not prefetch are not fetched from Cursor's KV store. `turns`
are read only when the JSON prompt is missing or empty; they are not
a second copy of a prompt that already resolved. Edit
`replace_all` is not offered and not encoded. Cursor 3.27.22's Pi edit
arm, in `cursor-agent-exec/dist/main.js`, is `PiEditExecArgs|1 path 9|2 edits`
and `PiEditReplacement|1 old_text 9|2 new_text 9`. That replacement has
no bool, so a field number for `replace_all` would be a guess. Tool results that
never arrive on the run body still leave the turn after the exec; the model
sees them only on a later run's `conversation_state`. A shell stream
chunk is not that result. Append
sequence numbers are not reordered, and a run's session is not dropped
when the stream ends. HTTP/2 is not intercepted;
`cursor.general.disableHttp2` is set so Cursor uses HTTP/1.1 after it
restarts. The gateway still does not speak Cursor's protocol on `:3425`.
The machine-wide trust store is not written. On Linux the system bundle
stays a command in the notice, because writing it needs root. Disconnect
does not remove the CA.
