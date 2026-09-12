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
