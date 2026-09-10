# BuiltinB Parser Plugin

BuiltinB is an independently built Parser plugin used to validate the WeKnora
external plugin framework. It is deliberately small: it accepts `md`,
`markdown`, and `txt`, and returns Markdown text through the public
`pkg/pluginapi` SDK.

## Build

From this directory:

```powershell
go test ./...
go build -o builtinb.exe .
```

The `replace` directive in `go.mod` points at the current WeKnora checkout for
local integration. Remove it when publishing the plugin and depend on a
released SDK version instead.

## Install

Keep `plugin.yaml` and `builtinb.exe` together, then point the host at the
parser plugin root:

```powershell
$env:WEKNORA_PLUGIN_DIR_PARSER = "D:\weknora-plugins\parser"
```

The host discovers `plugin.yaml`, starts `builtinb.exe`, performs the shared
handshake and health checks, and registers the engine as `builtinb`.

## Configuration

The host UI reads `config_schema` from `plugin.yaml`. Values are persisted per
tenant and per plugin, then arrive in `ParserEngineOverrides`:

- `trim_whitespace`: defaults to `true`;
- `title_prefix`: optional text added inside the first Markdown heading, while
  preserving its Markdown heading syntax.

No WeKnora frontend or internal package is imported by this plugin.
