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
| Which models | Magpie's agent catalog (`provider.Catalog`), one row per entry. The display name is the model's name. The route id is the catalog id the gateway already routes (`provider/model`, or a group's id). | [`from.go`](../../internal/cursoride/from.go) |
| What an inference id means | A bare route id, a variant string (`id[context=…,reasoning=…,fast=…]`) or a legacy slug (`id-200k-high-fast`) names that catalog entry. Anything else is Cursor's own model. | [`route.go`](../../internal/cursoride/route.go) |
| The gateway request | An invoke's messages become a `/v1/chat/completions` body: `model` is the route id, `reasoning_effort` is the level Cursor sent, `stream` is true. | [`compile.go`](../../internal/cursoride/compile.go) |
| The catalog HTTP call | `Handler` answers the five paths above. When an upstream reply is supplied it appends magpie's rows; otherwise it answers with those rows alone. | [`handler.go`](../../internal/cursoride/handler.go) |

`Handler` is not mounted on the gateway listener. The gateway speaks the
agent APIs in [gateway routing](gateway-routing.md). Ordinary Cursor does
not call those. Nothing here listens, writes Cursor's `settings.json` or
`state.vscdb`, or sets `CURSOR_LOCAL_AGENT_BASE_URL`.

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

1. Something in front of Cursor handles `POST` `/aiserver.v1.AiService/AvailableModels` (and the usable-model and default-model paths) by calling `Handler`. `Models` should be `LiveCatalog`, which reads `provider.Catalog` on each request.
2. `Handler` asks `Upstream` for Cursor's own reply when one is set. A 2xx reply is decoded as a Connect unary frame when it is one, and magpie's protobuf is appended. A failed upstream, or no upstream, answers with magpie's rows only (`application/proto`, not a frame). A compressed frame is a 502: it is not treated as an empty catalog.
3. On a run request the proxy calls `Decide`. `Local` means the model id is in the catalog. Any other id is Cursor's, and the proxy forwards that run.
4. On the invoke for a local run the proxy calls `CompileChat` and `POST`s `Chat.JSON` to the gateway's `/v1/chat/completions` with `Authorization: Bearer <gateway.TokenFor(AgentID)>` (`cursor-ide`) and `X-Magpie-Session` from `Chat.Header` when the run named a conversation.

A catalog request larger than 32 MiB is refused. It is not read as empty, which would replace Cursor's list with magpie's rows alone.

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
chat body and the catalog HTTP replies. It does not open Cursor. The proxy
that would sit in front of the IDE is not in this tree, so a model does not
yet appear in Cursor's picker from a running magpie.

## Not this package

Writing Cursor's proxy settings, terminating TLS toward Cursor's hosts, and
streaming a gateway reply back as Cursor's inference frames are not
implemented. Neither is a settings page for which models to inject. The
gateway keeps serving the chat body this package builds; it does not grow
Cursor's protocol on `:3425`.
