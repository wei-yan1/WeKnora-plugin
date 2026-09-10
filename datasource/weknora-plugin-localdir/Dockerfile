# Build with the repository root as context:
# docker build -f plugins/weknora-plugin-localdir/Dockerfile -t weknora/localdir:dev .
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN go build -trimpath -o /out/weknora-plugin-localdir ./plugins/weknora-plugin-localdir

FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /out/weknora-plugin-localdir /weknora-plugin-localdir
ENTRYPOINT ["/weknora-plugin-localdir"]
