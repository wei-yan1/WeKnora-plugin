# WeKnora Local Directory Plugin

This is an independent datasource plugin example for the WeKnora plugin
framework. It reads only the configured local directory, declares
`network: none`, and uses `file:<normalized-relative-path>` as its stable
external ID.

Build the **Linux binary** — this is what `plugin.yaml`'s `entrypoint`
(`./weknora-plugin-localdir`) points at, and what the host actually runs under
WSL / Docker:

```bash
cd datasource/weknora-plugin-localdir
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o weknora-plugin-localdir .
```

Windows (local debugging only):

```powershell
cd datasource/weknora-plugin-localdir
go build -o weknora-plugin-localdir.exe .
```

Build the isolated image (`Dockerfile` expects the repository root as context):

```powershell
docker build -f datasource/weknora-plugin-localdir/Dockerfile -t weknora/localdir:dev .
```

The host starts the process/container and supplies `WEKNORA_PLUGIN_ADDR`. The
incremental cursor contains a SHA-256 for every previously observed file, so a
change to one file returns only that file; a missing file returns a deletion
item.

## File size limit

Files above `maxFileBytes` (32 MiB) are rejected *before* being read. The batch
path packs every item into a single gRPC response and the host/SDK message limit
is 50 MB, so an oversized file would otherwise blow up the whole sync with an
opaque transport error.

An oversized or unreadable file is reported as a **failure placeholder** — an item
with no content and `Metadata["error"]`, the same convention the built-in Yuque /
Feishu connectors use. The host counts it as a failed document, writes it to the
sync log, and, because a failure was recorded, keeps the previous cursor so the
next incremental run retries it instead of skipping it forever. Such a file is
also still treated as *present*: it is deliberately not turned into a deletion
tombstone.
