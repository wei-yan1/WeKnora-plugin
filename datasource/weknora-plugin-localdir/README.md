# WeKnora Local Directory Plugin

This is an independent datasource plugin example for the WeKnora plugin
framework. It reads only the configured local directory, declares
`network: none`, and uses `file:<normalized-relative-path>` as its stable
external ID.

Build from the repository root:

```powershell
go build -o weknora-plugin-localdir.exe ./plugins/weknora-plugin-localdir
docker build -f plugins/weknora-plugin-localdir/Dockerfile -t weknora/localdir:dev .
```

The host starts the process/container and supplies `WEKNORA_PLUGIN_ADDR`. The
incremental cursor contains a SHA-256 for every previously observed file, so a
change to one file returns only that file; a missing file returns a deletion
item.
