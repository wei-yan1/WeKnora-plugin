# TARily Web Search Plugin

An external Web Search provider that wraps the [Tavily Search API](https://tavily.com/).

It is intentionally aliased as **`TARily`** (`metadata.provider_type: TARily`) so it
can coexist with the built-in Tavily provider (whose type is `tavily`) in the same
WeKnora deployment — this plugin exercises the external plugin framework rather than
the built-in registry.

## What it does

Implements the `search` extension point (`extension_type: search`): it takes a query,
forwards it to `POST https://api.tavily.com/search`, and returns the result list
(title / url / snippet / content / source / published date) to the host for fusion
and citation.

The API key, base URL, and proxy URL are forwarded by the host on every request;
the plugin keeps no tenant state.

## Build

From the repository root:

```powershell
Push-Location search/weknora-plugin-tarily
go build -buildvcs=false -mod=mod -o weknora-plugin-tarily.exe .
Pop-Location
```

For Linux:

```bash
cd search/weknora-plugin-tarily
go build -buildvcs=false -mod=mod -o weknora-plugin-tarily .
```

## Configure

| Field | Required | Description |
|---|---|---|
| `api_key` | yes | Tavily Search API key |
| `base_url` | no | API endpoint override (defaults to `https://api.tavily.com/search`) |
| `proxy_url` | no | Optional HTTP/HTTPS proxy for outbound requests |

The plugin requires outbound network access to `api.tavily.com`
(`permissions.network: allowlist`).
