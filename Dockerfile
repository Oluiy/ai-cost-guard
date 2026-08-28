# Pinned to match go.mod's toolchain directive — Go's own Docker images
# tag by exact version, so drifting this independently is how you get a
# build that works locally and fails in CI on a stdlib difference.
FROM golang:1.26-alpine AS build
WORKDIR /src

# go.mod/go.sum copied and downloaded before the rest of the source, so
# this layer only re-runs (and re-downloads every dependency) when a
# dependency actually changes, not on every source edit.
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# CGO_ENABLED=0: a static binary with no libc dependency, which is what
# lets the next stage be `scratch` (nothing installed, not even glibc)
# instead of needing a distro base image just to satisfy a dynamic link.
# -ldflags "-s -w" strips debug symbols; the binary works identically,
# just smaller — there's no in-container debugging happening in `scratch`.
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /fitguard ./cmd/fitguard

FROM scratch
# scratch has no trust store of its own. fitguard's entire job is calling
# OpenAI/Anthropic/Gemini/Groq/Together over HTTPS, so without this every
# upstream request fails TLS verification (x509: certificate signed by
# unknown authority) rather than the image just being minimal.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /fitguard /fitguard
EXPOSE 8787
# Split so `docker run fitguard init` (or any other subcommand) still
# works — ENTRYPOINT is the binary, CMD is only the *default* arguments,
# overridden by anything passed after the image name.
ENTRYPOINT ["/fitguard"]
CMD ["run", "--config", "/data/config.yaml"]
